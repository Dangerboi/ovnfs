# Limitations

- This is not a sandbox: absolute paths and symlinks can reach the host.
- The view is best effort, not a frozen snapshot: a source file the user
  rewrites while the agent is reading it can show either version. Local
  paths are copied in the background after the checkout opens: an edit the
  folder makes in the moments before, or before the folder watcher reports
  one made earlier, can show.
- Cache paths (build output, `node_modules`) are the folder's own files,
  shown while the folder has not changed them since the checkout opened. One
  the folder rewrites or removes later disappears from the checkout: a build
  in the folder can take an artifact away under a build running in a
  checkout, which then fails to open it and rebuilds it when run again, and
  a dependency install in the folder removes the replaced packages from the
  checkouts already open (see [configuration](configuration.md#classes)).
- A folder on another volume than the state gets its cache directories (and,
  in default `local: virtual`, `local` directories) empty in checkouts;
  standalone virtual `local` files (such as `.env`) still work across volumes.
  With opt-in `local: frozen`, local paths are byte-copied rather than
  reflinked across volumes.
- Over NFS (macOS), a checkout process reading a file inside a `local`
  directory gets `ESTALE` when the host replaces or deletes that file; edits
  in place and standalone `local` files such as `.env` are not affected.
- A tracked file the user changes after open is read from the repository's
  objects; pruning those objects can make it unreadable.
- Files from git (`--rev`, `--take` from the index or a revision) go through
  only Git LFS's checkout conversion. One git would write with CRLF line
  endings, an expanded `ident`, a `working-tree-encoding`, another filter
  driver's smudge, or LFS pointer extensions shows its blob as git stores it.
  An attached copy of such a checkout shows LFS pointers, not objects. A
  rename or metadata change that copies up an LFS file whose object is not in
  the repository's LFS storage waits for its download while holding the
  checkout's lock, as remote checkouts wait for content. On Windows a
  `git lfs smudge` that times out may keep downloading after the read failed.
- ovnfs reads git repositories with its own implementation, not a git
  installation. It reads the system configuration and attributes from
  `GIT_CONFIG_SYSTEM` or `/etc/gitconfig` and `/etc/gitattributes` (none on
  Windows), not from the prefix a particular git was installed to (such as
  Homebrew's or Git for Windows'), and cannot expand `%(prefix)/` paths.
  Fetches (`apply --branch`, the remote's git mirror) run `git fetch` when git
  is on `PATH`. Without git, ovnfs fetches itself over the same transports,
  finding credential helpers named in configuration on `PATH` or in
  `GIT_EXEC_PATH`, and reading client certificates (`http.sslCert`) only as
  PEM or DER files.
- Remote checkouts have no git directory and cannot be attached onward. Bring
  their edits back with `pull` on the source machine, then `apply` the local
  checkout to its folder. `fstat` on a lazily fetched open file reports the
  cached blob's metadata; path-based stat is exact.
- A link is one ssh session per checkout; cold reads of many files are bound by
  its throughput.
- Sockets and FIFOs can be made in NFS and FUSE checkouts, not SMB or WinFsp
  ones, and stay in the checkout: like a folder's own, they never reach
  `diff`, `apply` or a snapshot's tree. Device nodes are refused.
- NFSv3 locks are client-local. Explicit NFSv4 clients use byte-range locks
  held in the daemon's memory, lost when it restarts. SMB has no hard links
  and no durable handles.
- NFSv3 cannot push cache invalidations. The daemon refreshes its own mount's
  client for the paths a `pull` changed (see
  [platforms](platforms.md#nfs)), so those show at once; a reattach, which
  replaces whole trees, can remain cached below their roots until the client's
  attribute timeout (up to 60 s), as can any change on a machine that mounted
  the export itself. NFSv4 read delegations reduce that window for opened
  files, but do not cover directories or files only inspected with `stat`.
- One SMB daemon per Windows logon session: the SMB client keeps one set of
  credentials per server name, whatever the port, so a second daemon, whose
  state directory holds another password, cannot map `\\127.0.0.1\ovnfs`
  (error 1219). Run further daemons with `--proto winfsp`.
- WinFsp mounts get no recalls or change notifications for changes made
  behind them (`pull`, reattach): they show once the one-second
  metadata cache expires, and directory watchers miss them.
