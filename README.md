# dibs

Share one local Docker stack between several git worktrees, one holder at a time.

You work on several tickets at once, each in its own git worktree, often with a coding agent per worktree. Unit tests can run anywhere. But testing a change against a *running* stack (a browser, e2e, requests to a port) needs the containers to run that worktree's code. The containers are shared, so without coordination sessions quietly take them from each other and test the wrong code.

dibs is that coordination. It knows nothing about your project: it works from the containers themselves, so it fits compose, `docker run`, or any wrapper script.

```
$ dibs take browser web --wait
dibs: waiting: web held by ticket-a (app-ticket-a) since 14:02 (lease ends 14:32); position 1 in line
dibs: ticket-b holds browser, web until 15:01
dibs: web: waiting up to 5m0s for 1 running exec(s) from other sessions to finish: pytest -q
dibs: web: recreated, now serving ticket-b
dibs: web: it was serving ticket-a; commands that were still running in it and files any session copied into it are gone
dibs: ready. Run `dibs check` after each test batch and `dibs pass` when done testing.
```

## How it works

- **Swap by recreate.** `dibs take web` reads `web`'s configuration from the Docker Engine API and recreates it under the same name. Every bind mount that points into your repository points at your worktree instead. Networks, aliases, ports, environment, labels and volumes are kept, and anonymous volumes such as `node_modules` are re-attached, not recreated. There is never a second copy of a container.
- **Baseline.** The first time dibs touches a container it saves its configuration as the baseline, which `dibs restore` returns to. If something else recreates the container later (your usual `up`/`rebuild` tooling), dibs adopts that as the new baseline.
- **Queue and leases.** Resources are container names plus virtual locks such as `browser`.
  - A `take` gets all of its resources or none, in FIFO order, so it cannot starve. A worktree may take more while it holds something; if earlier waiters are waiting for what it holds, it goes ahead of them for the rest, since they cannot move until it is done. A take that would deadlock is refused.
  - `take --wait` waits until it is granted unless given `--timeout`; a take that times out loses its place in line, and dibs warns up front when the queue makes that likely.
  - Leases expire (30 minutes by default) and can be renewed.
  - A holder whose session makes no tool calls for 20 minutes while someone waits for what it holds loses those leases. A renew loop running in the background does not count as activity.
  - To keep containers without tool calls on purpose (the user is testing, or the session is waiting for the user), `dibs hold --note '<why>'` puts the leases on hold for a bounded time and shows the reason to everyone waiting.
  - Waiters that die are dropped from the line.
  - Humans can `grab` a resource, which pins it until they `drop` it.
- **Lazy handoff.** `dibs pass` does not restart anything. The container keeps the last holder's code until the next take, so each handoff costs one restart and re-taking your own worktree costs none.
- **Polite swaps.** Before recreating a container, dibs waits up to 5 minutes for `docker exec` commands running in it, which are usually another session's unit tests, and prints what it is waiting on.
- **Readiness.** A take returns once each container is ready:
  - its Docker healthcheck passes; or
  - the configured log line, HTTP or TCP probes pass; or
  - it has been running for a few seconds.
- **Failed and slow starts.** A container that exits during startup, keeps crashing (restarted 3 times while dibs waits), or logs the configured failure pattern has failed, and dibs says so at once instead of waiting out the timeout. One that is still running but not ready when its timeout passes is *starting*, not failed: it keeps going, and `dibs take` again, `dibs check` and `dibs status` re-check it. Either way the taker keeps the lease, so nobody else silently gets a broken container, and the take still switches the rest of its containers. Passing a container that failed to start puts it back on the baseline.
- **Stopping a take.** `dibs cancel` stops the worktree's take wherever it is. In line, it leaves the line. Mid-swap, the take finishes the recreate it is in (a container is never left half-recreated), stops waiting for readiness, and gives back everything it acquired; containers already switched keep running the worktree like after a pass. Killing a background take does the same as far as it can in the second or so the shell allows, so `dibs cancel` is the safe way. `dibs restore` holds the container while it works, so no take can recreate it at the same time.
- **Dropped changes.** The first time dibs recreates a container it did not create, it lists files changed inside it outside volumes (packages installed at runtime, say) and warns that the recreate drops them, since the baseline snapshot cannot capture them.
- **Baseline sync.** The baseline checkout drifts behind its remote branch while shared databases move on, until baseline containers stop starting. `dibs baseline` shows how far behind it is, and `dibs baseline sync` fetches and fast-forwards it. It refuses when the checkout has local changes or commits of its own, when any container running the baseline is leased or being recreated, or when a migrations check objects to the new revision. With `sync: auto`, a restore tries a sync first.
- **Migration checks.** A container can declare a command that says whether starting it on a given checkout is safe for the shared stores, for example because its startup would apply migrations. dibs runs it before a take recreates the container on a worktree (when queueing, and again when its turn comes), before restoring it, before restarting it with `dibs refresh`, and before a baseline sync, and refuses without `--allow-migrations`. dibs knows nothing about any migration tool; the command does.
- **Refresh.** Some containers read host state once at startup, such as credentials, and keep the old copy after the host renews it. `dibs refresh` restarts the containers the config declares under `refresh.containers` (only those started before the newest `refresh.watch` file changed, unless `--all`), after an optional `refresh.check` on the host. It waits for running execs, skips containers held by other worktrees, runs their migrations checks first, and tells sessions that used them, so agents can fix stale credentials without a human restarting shared containers.
- **Checking.** `dibs check` confirms you still hold everything and that each live container is still the one dibs created for your worktree. It catches expired leases, human takeovers and containers recreated behind dibs' back.

### Claude Code integration

- **Guard (PreToolUse).** Blocks tool calls that would take something from another worktree:
  - restarting, recreating or re-pointing a held container, including through your own wrapper commands (configurable patterns);
  - driving the browser (or an e2e run) before the containers the session took run its worktree; containers it did not take are left alone, whatever they run. A session that took nothing is asked to take `browser` and what its change touches while the stack runs someone else's code;
  - hitting a container's published port while it serves someone else;
  - switching branches in another worktree;
  - editing files or changing branches in the baseline checkout (the checkout the containers run by default, usually the main clone), including from a session that started there. That session is told to create a worktree instead.

  - taking containers for a worktree other than the session's own working directory (the session would not be recognised as the holder), with a pointer to EnterWorktree.

  It prints nothing unless it denies, so your normal permission prompts still apply, and it fails open on its own errors. It is a safety net, not a security boundary.
- **Primer (SessionStart and SubagentStart).** Sessions in a repository with managed containers start knowing the rules and who holds what. A session that starts in the baseline checkout is told to create a worktree before changing anything, and every session is reminded that databases are shared. The primer is re-injected after compaction. Subagents get it too, with a reminder that they share their parent's leases (leases belong to the worktree) and must not pass what they did not take.
- **Notes (PostToolUse).** A session is told when a container it used was recreated while its command ran or since it last touched it ("copy again and rerun"). Background commands are covered too: the note arrives with the session's next command or file read, which is usually it reading the command's output. The session is also told when it touches a container someone else holds, before its lease runs out, who is waiting for what it holds, and, at its next tool call, that a lease ended without it passing (expired, released while idle, or pinned by a human) and who holds it now.
- **Activity.** The hooks record when each worktree last made a tool call. `dibs status` shows it for every holder, and waiters see when a holder went quiet.
- **Skill.** `dibs claude-setup` installs a skill with the full protocol.
- **Shared browser.** `dibs browser-mcp` runs chrome-devtools-mcp against a single Chrome with remote debugging, launching it on first use. Every session gets browser tools and one login, instead of the second session failing on a locked profile.

### Unit tests without a lease

Tests that run from a copy of the code inside a container do not need the container to serve your worktree. `dibs tmpdir` prints a path derived from the worktree's location, so two worktrees can never pick the same directory:

```bash
dir=$(dibs tmpdir)                                  # /tmp/dibs-app-ticket-b-3f2a9c1d
docker exec web sh -c "rm -rf $dir && mkdir -p $dir"
docker cp ./web/. web:$dir/
docker exec -w $dir web pytest -q
```

A swap still recreates the container, which deletes `$dir`. The exec grace period and the post-command notes exist for exactly that case.

## Install

```bash
make install            # builds bin/dibs and installs it to ~/go/bin (override with BIN=...)
dibs claude-setup       # installs the skill and prints the hooks and MCP command to add
```

Requires Go 1.24+, macOS or Linux, and a Docker daemon on a unix socket (or plain TCP).

## Configuration

`~/.dibs/config.yaml` (override with `DIBS_CONFIG`; state lives in `~/.dibs` or `DIBS_HOME`). Everything is optional.

```yaml
lease: 30m            # default lease length
wait_timeout: 0       # how long `take --wait` waits before giving up; 0: until granted
ready_timeout: 10m    # how long a recreated container may take to become ready
stop_timeout: 10s     # docker stop grace period
exec_grace: 5m        # wait this long for running execs before recreating
idle_release: 20m     # release leases others wait for after this long without tool calls (0: never)
hold: 30m             # default length of `dibs hold`
hold_max: 2h          # longest a hold can run before the lease ends
restore_on_pass: false

repos:                # main checkouts: primer shown here, treated as the baseline before anything is taken
  - ~/code/app
  - path: ~/code/other
    baseline:
      remote: origin    # default: the checked-out branch's remote
      branch: main      # default: the remote's default branch
      sync: manual      # or auto: try `dibs baseline sync` before restoring
      after_sync:       # host commands run in the checkout after a fast-forward
        - 'docker exec -w /app web npm ci'

virtual: [browser]    # resources that are plain locks, not containers

groups:
  ui: [browser, web, api]

containers:
  web:
    ready:
      log: 'compiled successfully'   # regex over the new container's logs
      http: 'http://localhost:3000/' # any response below 500
      tcp: 'localhost:3000'
      settle: 3s                     # used when there is no healthcheck and no probe
      fail: 'Failed to compile'      # regex over its logs that means it will not get ready
      timeout: 15m                   # this container's readiness timeout (default ready_timeout)
      max_restarts: 3                # restarts while waiting that count as a crash loop
    migrations:
      check: '~/.dibs/checks/pending-migrations.sh web'   # exit 0: safe to start this checkout
      paths: [db/migrations]         # what the check reads, so a sync can check a revision before checking it out

guard:
  browser_tools: [mcp__chrome-devtools__, mcp__playwright__]
  mutate_patterns:                   # {service} and {container} expand per held container
    - '\bmycli\s+(up|rebuild|restart|stop)\b.*\b{service}\b'
    - '\bmycli\s+mount\b'            # no placeholder: denied while anything is held by others
  use_patterns:                      # test runners that need the stack
    - '\bplaywright\s+test\b'
  ports: true                        # deny requests to ports of containers serving others
  protect_baseline: true             # deny edits and branch changes in the baseline checkout

refresh:                             # containers `dibs refresh` may restart
  containers: [api, worker]
  watch: [~/.config/cloud/credentials.json]   # restart only containers started before these changed
  check: 'cloud-cli auth print-token >/dev/null'   # must pass on the host first

browser:
  port: 9222
  profile: ~/.cache/dibs/chrome-profile   # any dedicated profile; keep it logged in
  chrome: ''                              # auto-detected
  mcp_command: [npx, -y, chrome-devtools-mcp@latest]
```

### Migration checks

`containers.<name>.migrations.check` runs on the host with `sh -c`, in the checkout the container would run, with:

- `DIBS_TREE`: that checkout. For a baseline sync it is a temporary directory holding only `paths` at the new revision.
- `DIBS_REV`: the commit, when known.
- `DIBS_CONTAINER`: the container name.
- `DIBS_ACTION`: `take`, `restore`, `refresh` or `sync`.

Exit 0 means starting the container on that checkout is safe. Anything else means it is not, and whatever the command printed is shown. A typical check compares the migrations in `DIBS_TREE` with the versions recorded in the shared database and prints the ones that would run, or the recorded versions the checkout does not know (the container would refuse to start). A check gets 2 minutes.

## Commands

| Command | |
| --- | --- |
| `dibs take <res\|group>... [--wait] [--timeout] [--lease] [--no-swap] [--allow-migrations] [--tree DIR]` | take and swap |
| `dibs pass [res...] [--restore]` | give back |
| `dibs cancel` | stop this worktree's take, in line or mid-swap, and give back what it got |
| `dibs check [res...]` | exit 0 if still yours and still serving you |
| `dibs renew [--lease]` | extend leases |
| `dibs hold --note TEXT [--for 30m]` | keep leases without tool calls for a while, shown to waiters |
| `dibs restore <container>... \| --all [--allow-migrations]` | back to the baseline (unheld containers only) |
| `dibs refresh [container...] [--all] [--allow-migrations]` | restart the containers declared under `refresh` |
| `dibs baseline [--fetch]` | how far the baseline checkout is behind, and what runs it |
| `dibs baseline sync [--stash] [--allow-migrations] [--no-fetch]` | fetch and fast-forward the baseline when it is safe |
| `dibs grab <res>... [--note] [--tree]` / `dibs drop [res...]` | human override |
| `dibs status [--json]`, `dibs line` | who holds what, the queue, recent swaps |
| `dibs tmpdir` | per-worktree scratch path |
| `dibs guard [--post]`, `dibs context` | Claude Code hooks |
| `dibs browser-mcp [args...]` | shared Chrome for chrome-devtools-mcp |
| `dibs claude-setup` | install the skill, print the hook config |

Exit codes: 0 yes, 2 no, 1 error.

## Caveats

- A swap recreates the container. Anything written to its filesystem outside volumes is gone, just as with any recreate.
- Bind sources reported through Docker Desktop's VM path (`/host_mnt/...`) are mapped to the host paths they stand for, and keep their prefix when re-pointed.
- Only bind mounts whose source is inside the same repository are re-pointed. A path that exists in the baseline checkout but not in your worktree (gitignored data, say) keeps its baseline source, and `take` says so.
- A container with no bind mount from the repository cannot serve a worktree. `--no-swap` still locks it.
- The database and other shared services stay shared. Schema changes made by one worktree are visible to all.
- The guard matches commands by pattern. It stops honest mistakes, not determined ones.

## Development

```bash
make test    # unit tests, plus Docker integration tests when a daemon and busybox:latest are available
make vet
```

See [docs/spec.md](docs/spec.md) for the design.
