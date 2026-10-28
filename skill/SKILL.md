---
name: dibs
description: Take a lease on shared local Docker containers before browser QA, e2e runs, or any test that needs a running container to serve this worktree's code, and give it back afterwards. Use whenever you are about to drive a browser against a local stack or test against a running container, when a container seems to run the wrong branch or was recreated unexpectedly, when a dibs hook blocked a command, or when asked who has a container.
---

# dibs: sharing local containers between worktrees

Several agent sessions work in different git worktrees of one repository and share a single set of local Docker containers. A container can only run one worktree's code at a time. dibs is the queue that decides whose, and it recreates containers with the holder's worktree mounted.

If `dibs` is not on PATH, this skill does not apply.

## The rule

Hold containers only while testing, never while writing code. Others may be waiting for them.

```bash
dibs take browser web api --wait   # run in the background; returns once they run YOUR worktree
# ... drive the browser, run e2e, hit the ports ...
dibs check                         # exit 0: results are from your code; exit 2: discard them
dibs pass                          # as soon as you finish testing
```

Groups from the dibs config expand to several resources (the session primer lists them), so `dibs take ui --wait` may cover the browser and every container a UI test needs.

Take everything you need in **one** `take`; it is all-or-nothing. If you later need one more container, take it: if others are waiting for what you hold, you go ahead of them for it. A take that would deadlock is refused.

Leases belong to the **worktree**, not to a session. A subagent working in the same worktree shares its parent's leases: it should use what the parent already holds, and never `dibs pass` something it did not take itself.

dibs knows your session only by its **working directory**, not by `cd` inside a command. Take, test, check and pass from the worktree your session runs in. To work in a different worktree, move the session there with EnterWorktree (`path`) instead of prefixing commands with `cd`; a `dibs take` for another worktree is refused, because the lease would not count as yours.

One worktree per session is enough. For a follow-up PR, switch branches inside it once the previous branch is pushed. Use a second worktree only when two branches must be checked out at the same time.

## If your session starts in the baseline checkout

The baseline checkout is the one the shared containers run by default (usually the main clone), and the session primer names it. Every session without a lease is testing that code, so:

- Read, search and plan there as much as you like.
- **Before editing any file, create one worktree for this session and move the session into it.** The EnterWorktree tool does both; pass it `path` to enter a worktree you created with `git worktree add ../<name> -b <branch>`. If you cannot move the session, ask the user to start a session in the worktree.
- Never switch branches, pull, stash or reset in the baseline checkout. The dibs hook blocks edits and branch changes there.

If `dibs take` says your worktree is the baseline checkout, you are in the wrong place: make a worktree and take from there.

## Waiting

`dibs take ... --wait` blocks until every resource is yours **and** each container has been recreated on your worktree and is ready. Other sessions may be ahead of you, so run it as a **background** command and let its completion wake you. Do not poll it and do not start testing before it returns. It waits as long as it takes; do not pass `--timeout` unless you must give up by a deadline, because a take that times out loses its place in line. Its output tells you:
- who you waited for;
- what each container was serving before;
- mounts that were kept from the baseline because your worktree lacks them.

Without `--wait`, `dibs take` answers immediately: exit 0 means granted, exit 2 means someone else has it. Tell the user who, rather than proceeding.

To stop a take you no longer want, in line or already swapping, run `dibs cancel`. Do not kill the background command instead: the shell gives it about a second, which may not be enough to finish recreating a container.

## Which resources

- `browser`: whenever you will use browser automation. There is one shared Chrome, and every session sees every tab: act only on tabs you opened, and close them when done. When you take it from someone else, tabs they left behind are closed, so you start with one blank tab; passing it closes your tabs too, so keep holding it while the user looks at a tab you opened for them. Opening a tab past the limit is refused: close the ones you are done with first. If the browser was not running, your next browser call relaunches it.
- Every container whose code your change touches. `dibs status` lists the containers dibs knows about; any container name works.
- A container you did not change can stay as it is. It may still run the code of the last worktree that held it, which is fine as long as your change does not touch it. Only what you take must run your worktree, so take every container your change touches; do not restore containers you did not take just to tidy up.

## Shared databases

Databases and other stateful services are not swapped: every worktree uses the same ones. A schema migration you apply is seen by every session and can stop containers built from other checkouts from starting. Ask the user before applying one, and remove test data you add.

If the repository declares migrations checks, `dibs take` runs them before recreating a container on your worktree and refuses when your branch would migrate a shared store (the output lists what would run). Ask the user before running it again with `--allow-migrations`.

## Stale credentials

Some containers read host credentials when they start and keep the old ones after the user renews them on the host (errors such as `invalid_grant`, or calls that hang). If the dibs config declares them, run `dibs refresh`: it restarts the declared containers that started before the credentials changed, skips any held by other worktrees, and runs migrations checks first. Do not restart shared containers any other way.

## When the baseline is behind

The baseline checkout can fall behind its remote branch while shared databases move on. Then a restored container may refuse to start, or `dibs restore` refuses because the container's migrations check objects.

- `dibs baseline` shows how far behind it is and which containers run it.
- `dibs baseline sync` fetches and fast-forwards it. It refuses when that is unsafe and says why: local changes in the checkout, a lease on a container running the baseline, or a migrations check.
- Ask the user before passing `--stash` or `--allow-migrations`; both change what every session runs.

## When a container fails to start on your worktree

If `dibs take` says a container is still starting, it is running your worktree but has not passed its readiness check within its timeout (a slow build, say). You still hold it. Run `dibs take <container>` again later, which re-checks it without recreating it, and only test once it is ready.

If `dibs take` says a container failed to start (it exited, keeps crashing, or logged a failure), you still hold it: others wait until you pass or your lease ends. Read its logs (`docker logs <container>`). A common cause is a branch that needs newer dependencies than the container's image has; you may install them inside the container you hold. Then run `dibs take <container>` again, which re-checks it without recreating it. If you give up, `dibs pass`: a container that failed to start goes back on the baseline when passed.

If a take warns that the original container had changes outside its volumes (for example packages installed at runtime), those are gone once it is recreated. If the code the container runs needed them, it may fail to start; tell the user, since the real fix is usually rebuilding the image.

## Before and after screenshots

To show how the app looked before your change, have the containers serve another ref instead of switching your worktree's branch:

```bash
dibs take ui --ref origin/main --wait                     # containers serve a checkout of origin/main
dibs take ui --ref origin/main --patch flags.patch --wait # same, with local patches applied (a feature flag, say)
# ... before screenshots ...
dibs take ui --wait                                       # back to your worktree
# ... after screenshots ...
```

The checkout is managed by dibs; never edit it. `git fetch` first if the ref should be current. `dibs check` passes while the containers serve the ref you asked for.

## Checking your results

A lease can expire, a human can take over, or someone can recreate a container behind dibs' back. Tests keep passing in all of these cases, just against the wrong code. So:

**After every batch of tests, run `dibs check`. If it exits 2, discard those results and say so.** Then take again.

Leases last 30 minutes by default. You will be warned a few minutes before yours ends. Run `dibs renew` if you are still testing.

## Keeping containers for the user

Sometimes the containers have to stay on your worktree while you make no tool calls: the user wants to test your change themselves, or you have stopped to wait for the user (to log in, restart something, answer a question). Then run:

```bash
dibs hold --for 1h --note "the user is testing the toolbar change"
```

- It keeps your leases for that long without tool calls, up to a limit the dibs config sets, and everyone waiting sees your note.
- Tell the user what you are holding and until when, and run `dibs pass` as soon as they are done.
- Never keep leases alive with a renew loop. While someone waits for what you hold, a worktree that makes no tool calls for a while (20 minutes by default) loses those leases, and renewing does not count as a tool call. Laptop sleep also stalls loops, so they fail silently.

If you are told that sessions are waiting for what you hold, pass what you are not testing with right now.

## Unit tests need no lease

Run them with `dibs test`, which starts a private copy of the container (same image, environment and installed dependencies) with your worktree mounted, runs the command, and removes the copy:

```bash
dibs test <container> -- <test command>
```

- Nothing another session does can kill it, and it cannot affect the shared container.
- It exits with your command's exit code. Read that code directly; piping into `tail` hides it.
- Only a few runs go at once across the machine, and none start while memory pressure is critical; if yours has to wait, it says so. Run long suites as background commands.
- The copy shares the container's dependency volumes. Do not install packages from it; that changes them for everyone.

The older way also works: copy your code into the shared container. Use a scratch directory unique to your worktree so you never collide with another session:

```bash
dir=$(dibs tmpdir)
docker exec <container> sh -c "rm -rf $dir && mkdir -p $dir"
docker cp ./<service-dir>/. <container>:$dir/
docker exec -w $dir <container> <test command>
```

- Copy source only. Reuse the container's installed dependencies (virtualenv, node_modules) instead of copying them; symlink them into `$dir` if the tooling needs them there.
- Never write into the container's own code directory; that is what other sessions are testing.
- **Another session's `dibs take` can recreate the container at any time.** That kills commands running in it and deletes `$dir`. dibs waits a few minutes for running `docker exec` commands to finish before recreating, and tells you afterwards if you were affected. When that happens, copy again and rerun. It is not a bug in your change.

## When the hook refuses a command

A dibs hook blocks commands that would take a container from another worktree:
- restarting, recreating or re-pointing a held container;
- driving the browser or an e2e run while a container you took has not switched to your worktree yet, or, if you took nothing, while the stack runs someone else's code;
- switching branches in another worktree.

The message is accurate. Follow it: usually `dibs take <what you need> --wait` in the background, then retry. Do not work around the guard (for example by renaming commands or using another tool to do the same thing).

If a resource is **pinned by a human**, wait or tell the user you are blocked. Never run `dibs drop` or `dibs grab`; those are for humans.

If the message says a path is **the baseline** for some containers, do not change branches or files there. Make the change in your own worktree and test it with `dibs take`.

## When memory runs out

If dibs says the machine's memory pressure is critical, containers and the browser get slow or stop answering. Pass what you are not testing with, close browser tabs you do not need, and hold off on builds and test runs. Never kill or relaunch the browser or a shared container to fix the slowness: a browser relaunch can lose the logins every session relies on.

## Notes you may receive

After a tool call, dibs may add a note that:
- a container was recreated while or since you last used it: re-copy and rerun if your command touched it;
- a container you just used is held by another worktree: its running app shows their code;
- your lease is about to end;
- other sessions are waiting for what you hold;
- your worktree no longer holds something it held (the lease expired, it was released while you were idle, or a human pinned it): results from it since then are not from your code.

These are facts about the shared environment. Act on them before interpreting test output.

## Commands

| Command | What it does |
| --- | --- |
| `dibs take <res\|group>... --wait` | Queue, take all at once, recreate containers on your worktree. Run in the background. |
| `dibs check` | Exit 0 if you still hold everything and the containers still serve you. Exit 2 otherwise. |
| `dibs pass` | Give everything back. The containers keep your code until the next holder takes them. |
| `dibs cancel` | Stop your take, in line or mid-swap, and give back what it got. |
| `dibs renew` | Extend your leases. |
| `dibs hold --for <time> --note <why>` | Keep your leases without tool calls for a while (the user is testing, or you wait for the user). |
| `dibs status` | Holders, what each container runs, the queue, recent swaps. |
| `dibs line` | The queue. |
| `dibs test <container> -- <command>` | Run tests in a private copy of a container with your worktree mounted. |
| `dibs tmpdir` | Your worktree's scratch path for copies inside containers. |
| `dibs restore <container>` | Put a container back on the baseline (unheld, or held by you). |
| `dibs refresh` | Restart declared containers that started before host credentials changed. |
| `dibs baseline` | How far the baseline checkout is behind its remote branch. |
| `dibs baseline sync` | Fast-forward the baseline checkout when it is safe. |

Exit code 2 always means "no", and 1 means something broke.

## Do not

- Hold containers while writing code.
- Report test results without a passing `dibs check`.
- Restart, rebuild, recreate or re-point shared containers yourself.
- Switch branches in any worktree but your own, or edit anything in the baseline checkout.
- Run `dibs grab` or `dibs drop`, or edit files under `~/.dibs`.
