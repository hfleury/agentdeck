# agentdeck

Run many Claude Code sessions from one terminal screen on Linux: sessions on
the left, the selected session in the middle (you type in Claude's own input
box), details on the right. Design: [`../agent-tui-research.md`](../agent-tui-research.md),
plan: [`../agentdeck-build-plan.md`](../agentdeck-build-plan.md).

This is the **base step** (Phase 1).

## Install (Arch)

```sh
sudo pacman -S --needed go tmux git
sudo pacman -S --needed ttf-nerd-fonts-symbols-mono libnotify fd   # icons, notifications, folder search
# optional: zoxide (its frequent folders show up as recent)
make install        # → ~/.local/bin/agentdeck
agentdeck
```

Start it from a normal shell, not from inside a Claude session. (If you do
start it from Claude, agentdeck removes the variables that would stop
transcripts from being saved.)

## Use

| Where | Key / action | Does |
|---|---|---|
| anywhere | `Ctrl-A` / `Ctrl-G` | sessions list / context panel; again → back to Claude |
| anywhere | `Ctrl-Space` | Claude's input box |
| anywhere | `Alt-K` | command window: deck commands, and prompts you sent in any session |
| anywhere | `Alt-q` or `Ctrl-B d` | leave; sessions keep running, `agentdeck` reattaches |
| sessions | click, `⏎` | open the session and type in it (an exited one is resumed first) |
| sessions | `j`/`k`, `space` | move; show a session but stay in the list |
| sessions | `n` / `w` | new session window / new in a git worktree (both join the group you are in) |
| sessions | `e` / `g` | your own name for the session (empty: its last prompt) / its group (`Tab` completes, empty: none) |
| sessions | `J` / `K` | move the session down / up within its group |
| group header | `⏎`, click / `e` | fold or unfold / rename the group |
| new session window | type, `Tab`, `↑`/`↓`, `⏎` | type a path to browse (`Tab` opens a folder) or a name to search your home with `fd`; recent folders listed; `Ctrl-T` model; `⏎` starts, double-click starts |
| sessions | `d` / `x` / `R` / `:` | review changes / close / restart (resumes the conversation) / command window |

### Context panel (right, Ctrl-G)

What you attach goes with your next prompt (through Claude's
`UserPromptSubmit` hook); pinned items go again whenever they change.

| Key | Adds / does |
|---|---|
| `a` | a file or folder (a folder outside the session's folder is granted with `/add-dir`) |
| `l` / `n` | lines (`path:40-72`) / a note |
| `h` / `m` / `r` | another session's conversation / memory (`CLAUDE.md` + memory files) / last result |
| `c` | copy another session's items |
| `p` `s` `S` `R` `x` | pin · share one · share all · send again · remove |
| `b` / `⏎` | budget (15k tokens by default) / view what Claude receives |

Also: `a` in the diff view adds the hunk on screen; `a` in the tool-call
browser adds that call's output. From a shell or Neovim:
`agentdeck add <path>[:40-72] [--pin]`, `agentdeck note <text>`,
`agentdeck context` (JSON).

Tool calls of the selected session are listed under the sessions; click
one, or press `t`, for its input and output.

You type in Claude's own box: its `/` and `@` completion, history (↑, Ctrl-R)
and paste handling all work as usual. Every prompt is also recorded by the
deck (title, cross-session history in Alt-K).

Deck commands (in Alt-K): `/new` (opens the folder window), `/worktree <branch>`,
`/close`, `/restart`, `/changes`, `/title [name]`, `/group [name]`. An earlier prompt chosen in
Alt-K is put into Claude's box without sending it.

Status icons: idle `○`, working (spinner), tool `⚙`, needs input `!`/bell, done
`✓`, exited `✗` (Nerd Font glyphs when a Nerd Font is installed; set
`AGENTDECK_ICONS=nerd|unicode|ascii` to choose). Bold title = changed since you
last looked.

## Files

| What | Where |
|---|---|
| state (SQLite), daemon log, generated tmux.conf and hook settings | `$XDG_STATE_HOME/agentdeck/` (`~/.local/state/agentdeck/`) |
| daemon socket | `$XDG_RUNTIME_DIR/agentdeck/agentdeck.sock` |
| tmux server | `tmux -L agentdeck` (your own tmux is untouched) |

## Develop

```sh
make test     # unit tests
make e2e      # real tmux + keys + paste + mouse, fake claude (scripts/fake-claude)
python3 scripts/smoke.py   # real Claude (Haiku, two tiny prompts)
```
