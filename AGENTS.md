# Working on Aft

Aft runs agents on a server while the terminal stays on the user's machine.
Read [the design](docs/contributing/plan.md) before changing architecture.

- Preserve unrelated changes. Never revert or reformat code outside the task.
- Never add real credentials, host names or machine paths.
- Prefer simple, direct implementations. Add abstractions only when needed.
- Document public contracts and non-obvious constraints or reasons.
  Omit comments that merely restate clear names or code.
  Update affected comments and docs with each behavior change.
- Add focused tests for meaningful behavior; avoid redundant smoke tests.
- Do not publish, push, tag, run releases or change the user's machine without
  explicit authorization. Local checks and builds do not publish.

## Code map

Aft has three parts. Keep new code inside one of them.

- `internal/terminal` is the part on the user's machine: input tokenizer,
  prediction, passthrough, reconnect, pasting (paste.go) and copy mode (the
  scrollback pager, in copymode.go).
- `internal/session` is the part on the server: one application in a
  pseudo-terminal, one client attached at a time, repaint on reattach, the
  server CLI (cli.go), and relaying bridge requests. `internal/session/install`
  embeds the server build for each platform and installs it on a host.
- `internal/bridge` is everything that crosses between the two apart from
  the terminal stream: the clipboard in all three directions (copy from the
  server, read on paste, pasted files), opening URLs, and the server-side
  shims. `bridge/transport` reaches a host through the user's OpenSSH with a
  shared master connection, or runs the server locally for the reserved host
  `local`; `bridge/udp` carries a session's frames over QUIC on UDP once SSH
  has introduced the two ends; `bridge/proto` frames bytes between terminal
  and session; `bridge/ports` mirrors listening ports.

Supporting packages, not parts of their own:

- `cmd/aft` is the user binary: dispatch, commands, and the launcher entry when
  invoked under an agent's name. `cmd/aft-server` is the server binary, as
  mosh-server is mosh's, cross-compiled per platform into `internal/session/install/embed` (gzipped)
  and carried inside aft for install. `cmd/aft/selftest*.go` contains the interactive checks, using private Aft
  state and a temporary project; the setup preview also isolates HOME.
- `internal/vt` is the terminal emulator both sides use: the terminal's shadow
  screen and the session's detached screen. It renders a screen back to escape
  sequences and answers terminal queries.
- `internal/launcher` owns launchers, the on/off decision and the shell PATH
  block. `internal/home` locates every directory Aft writes.
- `internal/ui` holds raw-mode interactive prompts for setup and config. It
  never uses a TUI framework, because Bubble Tea probes the terminal on import,
  which would corrupt the session client that shares it.
- `internal/config` resolves settings as project over host over global, and
  carries the launch wrapper that lets a user run the agent inside any
  environment (a container, a nix or mise shell).

Tests live beside the code. The server and end-to-end tests spawn the test
binary itself as the server and drive the client under a real pseudo-terminal.

## Contracts

- Server output passes through to the terminal byte for byte. Predictions are
  drawn only between escape sequences and are reconciled or repainted.
- Enter, control keys, pastes and escape sequences are never predicted.
- The input loop never writes to the network; sends are queued to a writer
  goroutine, so prediction and typing are never gated by a slow link.
- The prefix key (default Ctrl-], configurable, disableable) is the only key
  aft takes from the application, and only while enabled; prefix then [ opens
  copy mode, prefix then d detaches. Copy mode works on a snapshot and never
  captures the mouse, so native selection still works.
- Sessions survive a lost client. Aft never stops a session on its own.
- Only allowlisted environment variables cross to the server.
- Aft writes only the directories `internal/home` names on each machine plus
  the recorded shell block, and `aft uninstall` removes exactly that: `~/.aft`
  for what people read or edit (config, state, logs), `~/.local/share/aft` for
  plumbing (server, shims, launchers, sessions, sockets) on every platform,
  and a private `aft-<uid>` directory in the platform's temporary space for
  disposable files such as pasted files.
- Aft installs nothing but its own server, edits nothing but the shell block,
  and never runs sudo. Setup asks before editing dotfiles and can print the
  line instead.
- Nothing on the client path may write to the terminal at startup. Interactive
  prompts live in internal/ui and run only when called; no imported package may
  probe the terminal, or it corrupts the passthrough client.

Run `./x check` for changes and `./x release-check` for distribution work.
Cross-build success is not runtime verification; report limits honestly.
