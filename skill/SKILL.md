---
name: dibs
description: Take a lease on shared local Docker containers before browser QA, e2e runs, or any test that needs a running container to serve this worktree's code, and give it back afterwards. Use whenever you are about to drive a browser against a local stack or test against a running container, when a container seems to run the wrong branch or was recreated unexpectedly, when a dibs hook blocked a command, or when asked who has a container.
---

# dibs: sharing local containers between worktrees

Several agent sessions work in different git worktrees of one repository and share a single set of local Docker containers. A container can only run one worktree's code at a time. dibs is the queue that decides whose, and it recreates containers with the holder's worktree mounted.

If `dibs` is not on PATH, this skill does not apply.

## The rule

Hold containers only while testing, never while writing code.

```bash
dibs take browser web api --wait   # run in the background; returns once they run YOUR worktree
# ... drive the browser, run e2e, hit the ports ...
dibs check                         # exit 0: results are from your code; exit 2: discard them
dibs pass                          # as soon as you finish testing
```

Groups from the dibs config expand to several resources (the session primer lists them), so `dibs take ui --wait` may cover the browser and every container a UI test needs.

Take everything you need in **one** `take`. It is all-or-nothing, so it cannot deadlock. Taking more while holding something is refused if it could deadlock.

## Waiting

`dibs take ... --wait` blocks until every resource is yours **and** each container has been recreated on your worktree and is ready. Other sessions may be ahead of you, so run it as a **background** command and let its completion wake you. Do not poll it and do not start testing before it returns. Its output tells you:
- who you waited for;
- what each container was serving before;
- mounts that were kept from the baseline because your worktree lacks them.

Without `--wait`, `dibs take` answers immediately: exit 0 means granted, exit 2 means someone else has it. Tell the user who, rather than proceeding.

## Which resources

- `browser`: whenever you will use browser automation. There is one shared Chrome, and every session sees every tab: act only on tabs you opened, and close them when done.
- Every container whose code your change touches. `dibs status` lists the containers dibs knows about; any container name works.
- A container you did not change can stay on the baseline. The browser guard requires every container dibs manages to run either the baseline or your worktree, so a container still running someone else's worktree must be taken (or restored with `dibs restore`) first.

## Checking your results

A lease can expire, a human can take over, or someone can recreate a container behind dibs' back. Tests keep passing in all of these cases, just against the wrong code. So:

**After every batch of tests, run `dibs check`. If it exits 2, discard those results and say so.** Then take again.

Leases last 30 minutes by default. You will be warned a few minutes before yours ends. Run `dibs renew` if you are still testing.

## Unit tests need no lease

Tests that run from a copy of your code inside a container do not need that container to serve your worktree. Use a scratch directory unique to your worktree so you never collide with another session:

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
- driving the browser while the stack serves someone else's code;
- switching branches in another worktree.

The message is accurate. Follow it: usually `dibs take <what you need> --wait` in the background, then retry. Do not work around the guard (for example by renaming commands or using another tool to do the same thing).

If a resource is **pinned by a human**, wait or tell the user you are blocked. Never run `dibs drop` or `dibs grab`; those are for humans.

If the message says a path is **the baseline** for some containers, do not change branches or files there. Make the change in your own worktree and test it with `dibs take`.

## Notes you may receive

After a command, dibs may add a note that:
- a container was recreated while or since you last used it: re-copy and rerun if your command touched it;
- a container you just used is held by another worktree: its running app shows their code;
- your lease is about to end.

These are facts about the shared environment. Act on them before interpreting test output.

## Commands

| Command | What it does |
| --- | --- |
| `dibs take <res\|group>... --wait` | Queue, take all at once, recreate containers on your worktree. Run in the background. |
| `dibs check` | Exit 0 if you still hold everything and the containers still serve you. Exit 2 otherwise. |
| `dibs pass` | Give everything back. The containers keep your code until the next holder takes them. |
| `dibs renew` | Extend your leases. |
| `dibs status` | Holders, what each container runs, the queue, recent swaps. |
| `dibs line` | The queue. |
| `dibs tmpdir` | Your worktree's scratch path for copies inside containers. |
| `dibs restore <container>` | Put an unheld container back on the baseline. |

Exit code 2 always means "no", and 1 means something broke.

## Do not

- Hold containers while writing code.
- Report test results without a passing `dibs check`.
- Restart, rebuild, recreate or re-point shared containers yourself.
- Switch branches in any worktree but your own.
- Run `dibs grab` or `dibs drop`, or edit files under `~/.dibs`.
