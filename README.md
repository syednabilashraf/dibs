# dibs

Share one local Docker stack between several git worktrees, one holder at a time.

Each worktree (typically one coding-agent session per worktree) calls dibs on the containers it needs before testing against them. dibs recreates those containers with the worktree's code mounted, queues everyone else, and guards against sessions taking containers from each other.
