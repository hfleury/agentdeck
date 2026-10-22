#!/usr/bin/env python3
"""End-to-end test of the base step: real tmux, real keys, fake claude.

Everything is isolated: its own tmux server, state dir, socket and
~/.claude, all under a temp dir that is removed afterwards.

    python3 scripts/e2e.py [-v]
"""
import json, os, shutil, sqlite3, subprocess, sys, tempfile, time

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
BIN = os.path.join(ROOT, "bin", "agentdeck")
VERBOSE = "-v" in sys.argv

tmp = tempfile.mkdtemp(prefix="agentdeck-e2e-")
server = f"agentdeck-e2e-{os.getpid()}"
env = dict(os.environ)
for k in list(env):
    if k in ("TMUX", "TMUX_PANE", "CLAUDECODE") or k.startswith("CLAUDE_CODE_"):
        del env[k]
env.update({
    "AGENTDECK_TMUX": server,
    "AGENTDECK_STATE_DIR": os.path.join(tmp, "state"),
    "AGENTDECK_SOCK": os.path.join(tmp, "agentdeck.sock"),
    "AGENTDECK_CLAUDE": os.path.join(ROOT, "scripts", "fake-claude"),
    "AGENTDECK_NOTIFY": "off",
    "AGENTDECK_NO_ATTACH": "1",
    "AGENTDECK_ICONS": "unicode",
    "CLAUDE_CONFIG_DIR": os.path.join(tmp, "claude-home"),
    "FAKE_CLAUDE_LOG": os.path.join(tmp, "fake.log"),
})

passed, failed = 0, []

def check(name, ok, detail=""):
    global passed
    if ok:
        passed += 1
        print(f"  ok   {name}")
    else:
        failed.append(name)
        print(f"  FAIL {name}  {detail}")

def run(*args, check_rc=True):
    r = subprocess.run(args, env=env, capture_output=True, text=True, timeout=30)
    if check_rc and r.returncode != 0:
        raise RuntimeError(f"{args}: {r.stderr.strip()}")
    return r.stdout

def tm(*args, check_rc=True):
    return run("tmux", "-L", server, *args, check_rc=check_rc)

def deck(*args):
    return run(BIN, *args)

def state():
    return json.loads(deck("state"))

def wait(fn, timeout=8.0, step=0.1):
    end = time.time() + timeout
    last = None
    while time.time() < end:
        try:
            last = fn()
            if last:
                return last
        except Exception as e:  # noqa: BLE001
            last = e
        time.sleep(step)
    return None

def capture(pane):
    return tm("capture-pane", "-p", "-t", pane)

def fake_log():
    try:
        return [json.loads(l) for l in open(env["FAKE_CLAUDE_LOG"])]
    except FileNotFoundError:
        return []

def logged(kind, text):
    return any(e["kind"] == kind and e.get("text") == text for e in fake_log())

def keys(pane, *k):
    tm("send-keys", "-t", pane, *k)

def type_text(pane, text):
    tm("send-keys", "-t", pane, "-l", text)

def paste(pane, text):
    tm("set-buffer", "-b", "e2e", text)
    tm("paste-buffer", "-p", "-b", "e2e", "-t", pane)

def click(pane, x, y, button=0):
    """Inject an SGR mouse click (x, y are 0-based) as raw bytes."""
    seq = f"\x1b[<{button};{x+1};{y+1}M\x1b[<{button};{x+1};{y+1}m"
    tm("send-keys", "-t", pane, "-H", *[f"{b:02x}" for b in seq.encode()])

def session_by_title(title):
    return next((s for s in state()["sessions"] if s["title"] == title), None)

def active():
    st = state()
    return next((s for s in st["sessions"] if s["id"] == st["active"]), None)

def git_repo(path):
    os.makedirs(path)
    for c in (["init", "-q", "-b", "main"], ["config", "user.email", "t@t"], ["config", "user.name", "t"]):
        subprocess.run(["git", "-C", path, *c], check=True)
    open(os.path.join(path, "README.md"), "w").write("hello\n")
    subprocess.run(["git", "-C", path, "add", "."], check=True)
    subprocess.run(["git", "-C", path, "commit", "-qm", "init"], check=True)

def claude_pane():
    return active()["pane"]

def history():
    db = sqlite3.connect(os.path.join(env["AGENTDECK_STATE_DIR"], "state.db"))
    try:
        return [r[0] for r in db.execute("SELECT text FROM history ORDER BY id")]
    finally:
        db.close()

def focused_pane():
    return tm("display", "-p", "-t", "deck", "#{pane_id}").strip()

def main():
    repo = os.path.join(tmp, "api")
    git_repo(repo)

    print("layout")
    deck()  # launcher without attach: tmux server, daemon, layout
    roles = {}
    for line in tm("list-panes", "-t", "deck", "-F", "#{pane_id}\t#{pane_start_command}").splitlines():
        pid, cmd = line.split("\t", 1)
        for role in ("sidebar", "prompt", "context", "empty"):
            if f"ui {role}" in cmd:
                roles[role] = pid
    check("deck has three panes: sessions, center, context (no bottom prompt)",
          set(roles) == {"sidebar", "context", "empty"}, str(roles))
    sidebar = roles["sidebar"]
    check("the keyboard starts in the center", focused_pane() == roles["empty"], focused_pane())
    check("sidebar renders", wait(lambda: "No sessions yet" in capture(sidebar)) is not None, capture(sidebar))

    print("sessions")
    sid = deck("new", repo).strip()
    check("new session starts and SessionStart hook arrives",
          wait(lambda: active() and active()["state"] == "idle" and active()["id"] == sid) is not None, str(state()))
    center = claude_pane()
    in_deck = tm("list-panes", "-t", "deck", "-F", "#{pane_id}").split()
    check("selected session is shown in the center", center in in_deck, str(in_deck))
    check("sidebar shows the session", wait(lambda: "new session" in capture(sidebar) and "api" in capture(sidebar)) is not None, capture(sidebar))

    print("typing in Claude's box")
    type_text(center, "hello world")
    keys(center, "Enter")
    check("prompt reaches Claude", wait(lambda: logged("message", "hello world")) is not None, str(fake_log()[-3:]))
    check("Stop hook → done", wait(lambda: active()["state"] == "done") is not None, active()["state"])
    check("title is the last message", active()["title"] == "hello world", active()["title"])
    check("it is in the deck's history", wait(lambda: "hello world" in history()) is not None, str(history()))
    type_text(center, "/model ahsjnda")
    keys(center, "Enter")
    check("slash commands typed in Claude reach it", wait(lambda: logged("command", "/model ahsjnda")) is not None)
    check("…and keep the title", active()["title"] == "hello world", active()["title"])

    print("status")
    type_text(center, "ask for permission")
    keys(center, "Enter")
    check("permission prompt → needs input", wait(lambda: active()["state"] == "input") is not None, active()["state"])
    check("sidebar shows the input icon", wait(lambda: "!" in capture(sidebar)) is not None, capture(sidebar))
    type_text(center, "y")
    keys(center, "Enter")
    check("answered → done", wait(lambda: active()["state"] == "done") is not None, active()["state"])

    print("layout protection")
    binds = tm("list-keys")
    check("tmux's pane-swapping bindings are gone", "swap-pane" not in binds and "rotate-window" not in binds, [l for l in binds.splitlines() if "swap" in l or "rotate" in l])
    tm("swap-pane", "-d", "-s", sidebar, "-t", center)  # what Ctrl+click used to do
    def left_is_sidebar():
        out = tm("list-panes", "-t", "deck", "-F", "#{pane_id} #{pane_left} #{pane_top}")
        return any(l.split()[0] == sidebar and l.split()[1:] == ["0", "0"] for l in out.splitlines())
    check("a moved pane is put back in its slot", wait(left_is_sidebar, 5) is not None, tm("list-panes", "-t", "deck", "-F", "#{pane_id} #{pane_left} #{pane_top}"))
    title = active()["title"]
    subprocess.run([BIN, "hook", "UserPromptSubmit"], env={**env, "AGENTDECK_SESSION": active()["id"]},
                   input=json.dumps({"prompt": "<task-notification> <task-id>b1</task-id> done"}), text=True, timeout=10)
    time.sleep(0.5)
    check("prompts Claude submits itself don't become the title", active()["title"] == title, active()["title"])
    check("…nor history", "<task-notification>" not in " ".join(history()))

    print("keys and mouse")
    sid2 = deck("new", repo).strip()
    wait(lambda: active()["id"] == sid2)
    tm("select-pane", "-t", sidebar)
    # Session 1 is on rows 2-3 (after the header), session 2 on rows 4-5.
    click(sidebar, 5, 2)
    check("clicking a session switches to it", wait(lambda: state()["active"] == sid) is not None, state()["active"])
    check("…and puts the keyboard in its Claude box", wait(lambda: focused_pane() == claude_pane()) is not None, focused_pane())
    tm("select-pane", "-t", sidebar)
    click(sidebar, 5, 5)
    check("clicking the subtitle line works too", wait(lambda: state()["active"] == sid2) is not None, state()["active"])
    tm("select-pane", "-t", sidebar)
    keys(sidebar, "k", "Space")
    check("space shows a session but stays in the list", wait(lambda: state()["active"] == sid) is not None and focused_pane() == sidebar, focused_pane())
    deck("send", "/close")  # closes session 1

    print("worktrees")
    wait(lambda: active() is not None)
    out = deck("send", "/worktree", "feat-x")
    wt = os.path.join(tmp, "api.worktrees", "feat-x")
    check("/worktree creates the worktree", os.path.isdir(wt), out)
    s = wait(lambda: active() if active() and active()["worktree"] == wt else None)
    check("the new session runs in it", s is not None and s["cwd"] == wt, str(active()))
    check("subtitle keeps the origin folder and shows the branch",
          wait(lambda: "feat-x" in capture(sidebar) and "api.worktrees" not in capture(sidebar)) is not None, capture(sidebar))
    open(os.path.join(wt, "new.txt"), "w").write("a\nb\n")
    check("git changes show in the subtitle", wait(lambda: "?1" in capture(sidebar), timeout=8) is not None, capture(sidebar))
    os.remove(os.path.join(wt, "new.txt"))
    out = deck("send", "/close")
    check("closing a clean worktree session removes the worktree", wait(lambda: not os.path.isdir(wt)) is not None, out)
    branches = subprocess.run(["git", "-C", repo, "branch"], capture_output=True, text=True).stdout
    check("…and keeps the branch", "feat-x" in branches, branches)

    print("exit and resume")
    s = wait(lambda: active())
    first = s["id"]
    type_text(claude_pane(), "remember this")
    keys(claude_pane(), "Enter")
    wait(lambda: active()["state"] == "done")
    type_text(claude_pane(), "exit")
    keys(claude_pane(), "Enter")
    check("claude exiting → exited", wait(lambda: active()["state"] == "exited") is not None, active()["state"])
    check("sidebar offers resume", wait(lambda: "⏎ resumes" in capture(sidebar)) is not None, capture(sidebar))
    r = subprocess.run([BIN, "send", "more"], env=env, capture_output=True, text=True)
    check("sending to an exited session is refused", r.returncode != 0 and "exited" in r.stderr, r.stderr)
    keys(sidebar, "R")
    check("R restarts with --resume", wait(lambda: any(e["kind"] == "start" and "--resume" in e["argv"] for e in fake_log())) is not None)
    check("restarted session is waiting again", wait(lambda: active()["state"] in ("done", "idle")) is not None, active()["state"])
    starts = sum(1 for e in fake_log() if e["kind"] == "start")
    type_text(claude_pane(), "exit")
    keys(claude_pane(), "Enter")
    check("claude exiting again → exited", wait(lambda: active()["state"] == "exited") is not None, active()["state"])
    keys(sidebar, "Enter")
    check("Enter on an exited session resumes it", wait(lambda: sum(1 for e in fake_log() if e["kind"] == "start" and "--resume" in e["argv"]) >= 2 and sum(1 for e in fake_log() if e["kind"] == "start") > starts) is not None)
    check("…and it is waiting again", wait(lambda: active()["state"] in ("done", "idle")) is not None, active()["state"])

    print("daemon restart and old decks")
    # A deck from before the change had a prompt pane at the bottom.
    old = tm("split-window", "-v", "-f", "-l", "8", "-t", sidebar, "-d", "-P", "-F", "#{pane_id}", "sleep 600").strip()
    db = sqlite3.connect(os.path.join(env["AGENTDECK_STATE_DIR"], "state.db"))
    lay = json.loads(db.execute("SELECT value FROM kv WHERE key='layout'").fetchone()[0])
    lay["prompt"] = old
    db.execute("UPDATE kv SET value=? WHERE key='layout'", (json.dumps(lay),))
    db.commit()
    db.close()
    tm("respawn-window", "-k", "-t", "sys:0", f"'{BIN}' daemon")
    time.sleep(1)
    check("state survives a daemon restart", wait(lambda: active() and active()["id"] == first) is not None)
    deck()  # what running `agentdeck` again does
    panes_now = tm("list-panes", "-t", "deck", "-F", "#{pane_id}").split()
    check("an old deck's bottom prompt pane is removed", old not in panes_now and len(panes_now) == 3, str(panes_now))
    check("panes reconnect", wait(lambda: "remember this" in capture(sidebar) and "offline" not in capture(sidebar)) is not None, capture(sidebar))

    context_section(repo)
    groups_section(repo)
    client_section(repo)

def groups_section(repo):
    print("names and groups")
    sb = role_pane("sidebar")
    a = active()["id"]
    tm("select-pane", "-t", sb)
    keys(sb, "e")
    type_text(sb, "API work")
    keys(sb, "Enter")
    check("e names a session", wait(lambda: active().get("name") == "API work" and "API work" in capture(sb)) is not None, capture(sb))
    ask(claude_pane(), "another prompt")
    check("…and the name stays when you send prompts", active().get("name") == "API work" and "API work" in capture(sb) and active()["title"] == "another prompt", str(active()))
    tm("select-pane", "-t", sb)
    keys(sb, "g")
    type_text(sb, "Backend")
    keys(sb, "Enter")
    check("g puts it in a group, shown as a header", wait(lambda: active().get("group") == "Backend" and "▾ Backend" in capture(sb)) is not None, capture(sb))
    b = deck("new", repo).strip()
    wait(lambda: active()["id"] == b)
    tm("select-pane", "-t", sb)
    keys(sb, "g")
    type_text(sb, "Ba")
    keys(sb, "Tab")
    time.sleep(0.2)
    keys(sb, "Enter")
    check("tab completes an existing group", wait(lambda: active().get("group") == "Backend") is not None, str(active().get("group")))
    def ids_in(group):
        return [x["id"] for x in state()["sessions"] if x.get("group") == group]
    check("a session joins at the end of its group", ids_in("Backend") == [a, b], str(ids_in("Backend")))
    keys(sb, "K")
    check("K moves it up within the group", wait(lambda: ids_in("Backend") == [b, a]) is not None, str(ids_in("Backend")))
    keys(sb, "J")
    check("J moves it down", wait(lambda: ids_in("Backend") == [a, b]) is not None, str(ids_in("Backend")))

    keys(sb, "Home", "Enter")
    check("Enter on a group header folds it", wait(lambda: "▸ Backend" in capture(sb) and "API work" not in capture(sb)) is not None, capture(sb))
    check("…the folded header shows how many sessions it has", "▸ Backend 2" in capture(sb), capture(sb))
    header_y = next(i for i, l in enumerate(capture(sb).splitlines()) if "Backend" in l)
    click(sb, 5, header_y)
    check("clicking the header unfolds it", wait(lambda: "▾ Backend" in capture(sb) and "API work" in capture(sb)) is not None, capture(sb))

    out = deck("send", "/worktree", "grp-x")
    check("a session started from a grouped one joins its group",
          wait(lambda: active()["worktree"].endswith("grp-x") and active().get("group") == "Backend") is not None, str(active()))
    deck("send", "/close")
    tm("select-pane", "-t", sb)
    keys(sb, "Home", "e", "C-u")
    type_text(sb, "Core")
    keys(sb, "Enter")
    check("e on a header renames the group", wait(lambda: state()["sessions"] and set(x.get("group") for x in state()["sessions"] if x.get("group")) == {"Core"}) is not None, str([x["group"] for x in state()["sessions"]]))
    deck("send", "/title", "temp name")
    check("/title names the active session", wait(lambda: active().get("name") == "temp name") is not None)
    deck("send", "/title")
    check("/title alone goes back to the last prompt", wait(lambda: active().get("name", "") == "") is not None, str(active()))
    deck("send", "/group")
    check("/group alone takes it out of its group", wait(lambda: active().get("group", "") == "") is not None, str(active()))

def ctx(sid=""):
    return json.loads(deck("context", sid))

def ctx_item(sid, pred):
    return next((i for i in ctx(sid)["items"] or [] if pred(i)), None)

def contexts():
    return [e for e in fake_log() if e["kind"] == "context"]

def context_for(prompt):
    return next((e["text"] for e in contexts() if e["prompt"] == prompt), None)

def ask(pane, prompt):
    """Type a prompt in Claude's box and wait for the whole turn."""
    before = sum(1 for e in fake_log() if e["kind"] == "turn_done")
    type_text(pane, prompt)
    keys(pane, "Enter")
    ok = wait(lambda: sum(1 for e in fake_log() if e["kind"] == "turn_done") > before) is not None
    wait(lambda: active()["state"] == "done")
    return ok

def role_pane(role):
    return next(l.split("\t")[0] for l in tm("list-panes", "-t", "deck", "-F", "#{pane_id}\t#{pane_start_command}").splitlines() if f"ui {role}" in l)

def context_section(repo):
    print("context: notes, files, lines")
    a = active()["id"]
    pa = claude_pane()
    cpane = role_pane("context")
    check("context panel starts empty", wait(lambda: "Nothing attached yet" in capture(cpane)) is not None, capture(cpane))
    deck("note", "we use sqlc, not GORM")
    check("a note shows under Next prompt", wait(lambda: "Next prompt" in capture(cpane) and "we use sqlc" in capture(cpane)) is not None, capture(cpane))
    ask(pa, "first question")
    c = context_for("first question")
    check("it goes with the next prompt you type in Claude", c is not None and "we use sqlc, not GORM" in c and "<agentdeck-context>" in c, str(c))
    check("…and then moves to Sent", wait(lambda: "Sent" in capture(cpane)) is not None and ctx_item(a, lambda i: i["kind"] == "note")["sent_at"] > 0, capture(cpane))
    ask(pa, "second question")
    check("a sent item doesn't go again", context_for("second question") is None, str(context_for("second question")))

    notes = os.path.join(repo, "notes.txt")
    open(notes, "w").write("alpha\nbeta\ngamma\n")
    deck("add", notes, "--pin")
    ask(pa, "q3")
    check("a pinned file goes with the next prompt", "alpha" in (context_for("q3") or ""), str(context_for("q3")))
    ask(pa, "q4")
    check("…not again while it is unchanged", context_for("q4") is None, str(context_for("q4")))
    open(notes, "w").write("alpha\nBETA2\ngamma\n")
    check("…the panel marks it to resend once it changes", wait(lambda: "↑" in capture(cpane)) is not None, capture(cpane))
    ask(pa, "q5")
    check("…and it goes again", "BETA2" in (context_for("q5") or ""), str(context_for("q5")))
    deck("add", notes + ":2-3")
    ask(pa, "q6")
    c6 = context_for("q6") or ""
    check("lines path:2-3 send only those lines", 'lines="2-3"' in c6 and "BETA2\ngamma" in c6 and "alpha" not in c6, c6)

    print("context: folders outside the session")
    lab = os.path.join(tmp, "homelab")
    os.makedirs(lab)
    open(os.path.join(lab, "docker-compose.yml"), "w").write("services: {}\n")
    deck("add", lab)
    check("the folder is granted with /add-dir when Claude's box is free",
          wait(lambda: logged("command", "/add-dir " + lab)) is not None, str([e for e in fake_log() if e["kind"] == "command"][-2:]))
    ask(pa, "q7")
    c7 = context_for("q7") or ""
    check("its file list goes with the next prompt", "docker-compose.yml" in c7 and "<folder" in c7, c7)

    print("context: budget")
    tm("select-pane", "-t", cpane)
    keys(cpane, "b")
    keys(cpane, "C-u")
    type_text(cpane, "600")
    keys(cpane, "Enter")
    check("b sets the budget", wait(lambda: ctx(a)["budget"] == 600) is not None, str(ctx(a)["budget"]))
    big = os.path.join(repo, "big.txt")
    open(big, "w").write("z" * 8000)
    deck("add", big)
    check("an item that doesn't fit is marked over the budget",
          wait(lambda: (ctx_item(a, lambda i: i["ref"] == big) or {}).get("error") == "over the budget") is not None, capture(cpane))
    ask(pa, "q8")
    check("Claude is told it was left out", "Left out, over the 600-token budget" in (context_for("q8") or ""), str(context_for("q8")))
    keys(cpane, "b")
    keys(cpane, "C-u")
    type_text(cpane, "15000")
    keys(cpane, "Enter")
    wait(lambda: ctx(a)["budget"] == 15000)

    print("tool calls")
    sb = role_pane("sidebar")
    ask(pa, "please fail now")
    check("tool calls show under the sessions list", wait(lambda: "Tool calls" in capture(sb) and "Bash" in capture(sb)) is not None, capture(sb))
    db = sqlite3.connect(os.path.join(env["AGENTDECK_STATE_DIR"], "state.db"))
    row = db.execute("SELECT status, output FROM events WHERE type='tool' ORDER BY id DESC LIMIT 1").fetchone()
    db.close()
    check("a failed call is recorded as failed, with its error", row and row[0] == "failed" and "FAIL TestLogin" in row[1], str(row))
    check("…and shown in red with ✗", wait(lambda: "✗" in capture(sb) and "go test" in capture(sb)) is not None, capture(sb))

    print("context: other sessions")
    open(os.path.join(repo, "CLAUDE.md"), "w").write("Use Postgres 16.\n")
    repo2 = os.path.join(tmp, "web")
    git_repo(repo2)
    b = deck("new", repo2).strip()
    wait(lambda: active()["id"] == b and active()["state"] == "idle")
    tm("select-pane", "-t", cpane)
    keys(cpane, "h")
    check("h lists the other sessions", wait(lambda: "Add the chat of" in capture(cpane)) is not None, capture(cpane))
    keys(cpane, "Enter")
    keys(cpane, "m")
    wait(lambda: "Add the memory of" in capture(cpane))
    keys(cpane, "Enter")
    check("conversation and memory items are added", wait(lambda: len(ctx(b)["items"] or []) == 2) is not None, str(ctx(b)))
    ask(claude_pane(), "what did they decide")
    cb = context_for("what did they decide") or ""
    check("the other session's conversation goes along", "<session-transcript" in cb and "first question" in cb, cb[:300])
    check("…and its memory (CLAUDE.md)", "Use Postgres 16." in cb and "not instructions" in cb, cb[-300:])
    deck("note", "from web")
    keys(cpane, "S")
    wait(lambda: "Share all items with" in capture(cpane))
    keys(cpane, "Enter")
    check("S shares the items with another session",
          wait(lambda: ctx_item(a, lambda i: i["kind"] == "note" and i["payload"] == "from web") is not None) is not None, str(ctx(a)))
    check("…but not a session's own conversation", ctx_item(a, lambda i: i["kind"] == "conversation") is None, str(ctx(a)))
    deck("send", "/close")
    db = sqlite3.connect(os.path.join(env["AGENTDECK_STATE_DIR"], "state.db"))
    left = db.execute("SELECT COUNT(*) FROM context_items WHERE session_id=?", (b,)).fetchone()[0]
    db.close()
    check("closing a session removes its items", left == 0, str(left))
    wait(lambda: active() and active()["id"] == a)

    print("context: restart keeps folders readable")
    keys(sb, "R")
    check("a restart passes --add-dir for outside folders",
          wait(lambda: any(e["kind"] == "start" and lab in e["argv"] for e in fake_log())) is not None,
          str([e["argv"] for e in fake_log() if e["kind"] == "start"][-1:]))
    wait(lambda: active()["state"] in ("idle", "done"))

def client_section(repo):
    """Popups need a terminal attached to the deck: attach one from a second
    tmux server and drive it through that terminal, like a real user."""
    print("with a terminal attached (125x45)")
    os.makedirs(os.path.join(repo, "svc"), exist_ok=True)
    tmux_outer("new-session", "-d", "-s", "o", "-x", "125", "-y", "45",
               f"env -u TMUX tmux -L {server} attach-session -t deck")
    check("terminal attached", wait(lambda: "Sessions" in outer_cap()) is not None, outer_cap())
    def widths():
        out = tm("list-panes", "-t", "deck", "-F", "#{pane_start_command}\t#{pane_width}")
        return {("sidebar" if "ui sidebar" in l else "context" if "ui context" in l else "x"): int(l.split("\t")[1]) for l in out.splitlines()}
    w = wait(lambda: (lambda x: x if x.get("sidebar") == 26 and x.get("context") == 23 else None)(widths()))
    check("side panes are a share of the width (26 and 23 of 125 columns)", w is not None, str(widths()))
    tmux_outer("resize-window", "-t", "o", "-x", "200")
    w = wait(lambda: (lambda x: x if x.get("sidebar") == 34 and x.get("context") == 32 else None)(widths()))
    check("…and follow a terminal resize, up to their maximum", w is not None, str(widths()))
    tmux_outer("resize-window", "-t", "o", "-x", "125")
    full_height = wait(lambda: all(int(h) >= 44 for h in tm("list-panes", "-t", "deck", "-F", "#{pane_height}").split()))
    check("every pane uses the full height", full_height is not None, tm("list-panes", "-t", "deck", "-F", "#{pane_height}"))

    n = len(state()["sessions"])
    sidebar = next(l.split("\t")[0] for l in tm("list-panes", "-t", "deck", "-F", "#{pane_id}\t#{pane_start_command}").splitlines() if "ui sidebar" in l)
    tm("select-pane", "-t", sidebar)
    keys(sidebar, "n")
    check("n opens the New session window", wait(lambda: "Start a session in" in outer_cap()) is not None, outer_cap())
    check("it lists the subfolders", wait(lambda: "svc/" in outer_cap()) is not None, outer_cap())
    tmux_outer("send-keys", "-t", "o", "-l", "sv")
    time.sleep(0.3)
    check("typing filters and shows where Enter starts", wait(lambda: "start in" in outer_cap() and "api/svc" in outer_cap()) is not None, outer_cap())
    tmux_outer("send-keys", "-t", "o", "Enter")
    check("Enter starts the session there", wait(lambda: any(x["cwd"].endswith("/api/svc") for x in state()["sessions"])) is not None, str([x["cwd"] for x in state()["sessions"]]))
    check("the window closes", wait(lambda: "Start a session in" not in outer_cap()) is not None, outer_cap())
    wait(lambda: active()["state"] == "idle")

    print("keys, through the terminal")
    tm("select-pane", "-t", claude_pane())
    tmux_outer("send-keys", "-t", "o", "C-a")
    check("Ctrl-A goes to the sessions list", wait(lambda: focused_pane() == sidebar) is not None, focused_pane())
    tmux_outer("send-keys", "-t", "o", "C-a")
    check("Ctrl-A again goes back to Claude", wait(lambda: focused_pane() == claude_pane()) is not None, focused_pane())
    context = next(l.split("\t")[0] for l in tm("list-panes", "-t", "deck", "-F", "#{pane_id}\t#{pane_start_command}").splitlines() if "ui context" in l)
    tmux_outer("send-keys", "-t", "o", "C-g")
    check("Ctrl-G goes to the context panel", wait(lambda: focused_pane() == context) is not None, focused_pane())
    tmux_outer("send-keys", "-t", "o", "C-Space")
    check("Ctrl-Space goes to Claude", wait(lambda: focused_pane() == claude_pane()) is not None, focused_pane())

    print("command window (Alt-K)")
    tmux_outer("send-keys", "-t", "o", "M-k")
    check("Alt-K opens it", wait(lambda: "Commands and earlier prompts" in outer_cap()) is not None, outer_cap())
    check("it lists deck commands and earlier prompts", wait(lambda: "/worktree" in outer_cap() and "please fail now" in outer_cap()) is not None, outer_cap())
    tmux_outer("send-keys", "-t", "o", "-l", "hello wo")
    time.sleep(0.4)
    tmux_outer("send-keys", "-t", "o", "Enter")
    check("an earlier prompt goes into Claude's box, not sent yet",
          wait(lambda: "Commands and earlier" not in outer_cap() and "hello world" in capture(claude_pane())) is not None
          and sum(1 for e in fake_log() if e["kind"] == "message" and e.get("text") == "hello world") == 1, capture(claude_pane()))
    check("the keyboard is in Claude's box", focused_pane() == claude_pane(), focused_pane())
    keys(claude_pane(), "Enter")
    check("Enter in Claude sends it", wait(lambda: sum(1 for e in fake_log() if e["kind"] == "message" and e.get("text") == "hello world") == 2) is not None)

    tmux_outer("send-keys", "-t", "o", "M-k")
    wait(lambda: "Commands and earlier prompts" in outer_cap())
    tmux_outer("send-keys", "-t", "o", "-l", "/wor")
    time.sleep(0.3)
    tmux_outer("send-keys", "-t", "o", "Enter")
    check("a command that needs an argument asks for it", wait(lambda: "/worktree " in outer_cap() and "Commands and earlier" in outer_cap()) is not None, outer_cap())
    tmux_outer("send-keys", "-t", "o", "-l", "feat-y")
    tmux_outer("send-keys", "-t", "o", "Enter")
    wt = os.path.join(tmp, "api.worktrees", "feat-y")
    check("…then runs it", wait(lambda: os.path.isdir(wt)) is not None and wait(lambda: "Commands and earlier" not in outer_cap()) is not None, outer_cap())

    tmux_outer("send-keys", "-t", "o", "M-k")
    wait(lambda: "Commands and earlier prompts" in outer_cap())
    tmux_outer("send-keys", "-t", "o", "-l", "/new")
    time.sleep(0.3)
    tmux_outer("send-keys", "-t", "o", "Enter")
    check("/new opens the folder window in the same popup", wait(lambda: "Start a session in" in outer_cap()) is not None, outer_cap())
    tmux_outer("send-keys", "-t", "o", "Escape")
    check("Esc closes it", wait(lambda: "Start a session in" not in outer_cap()) is not None, outer_cap())
    tmux_outer("send-keys", "-t", "o", "M-k")
    wait(lambda: "Commands and earlier prompts" in outer_cap())
    tmux_outer("send-keys", "-t", "o", "-l", "/model x")
    tmux_outer("send-keys", "-t", "o", "Enter")
    check("Claude's commands are refused there (they go in Claude's box)", wait(lambda: "not an agentdeck command" in outer_cap()) is not None, outer_cap())
    tmux_outer("send-keys", "-t", "o", "Escape")
    check("…and nothing new was started", len(state()["sessions"]) == n + 2, str(len(state()["sessions"])))
    wait(lambda: "Commands and earlier" not in outer_cap())

    print("context popups")
    a = active()["id"]
    ask(claude_pane(), "make a tool call")
    sb, cpane = role_pane("sidebar"), role_pane("context")
    tm("select-pane", "-t", sb)
    keys(sb, "t")
    check("t opens the tool calls", wait(lambda: "Tool calls" in outer_cap() and "hi from make a tool call" in outer_cap()) is not None, outer_cap())
    tmux_outer("send-keys", "-t", "o", "a")
    check("a adds the output to the context", wait(lambda: ctx_item(a, lambda i: i["kind"] == "tool_output")) is not None, outer_cap())
    tmux_outer("send-keys", "-t", "o", "q")
    wait(lambda: "a add output" not in outer_cap())

    tm("select-pane", "-t", cpane)
    keys(cpane, "g", "Enter")
    check("Enter shows what Claude receives", wait(lambda: "what Claude receives" in outer_cap()) is not None, outer_cap())
    tmux_outer("send-keys", "-t", "o", "q")
    wait(lambda: "what Claude receives" not in outer_cap())

    readme = os.path.join(active()["cwd"], "deploy-notes.md")
    open(readme, "w").write("ship it\n")
    keys(cpane, "a")
    check("a opens the Add to context window", wait(lambda: "Add to context" in outer_cap()) is not None, outer_cap())
    tmux_outer("send-keys", "-t", "o", "-l", "deploy")
    time.sleep(0.4)
    tmux_outer("send-keys", "-t", "o", "Enter")
    check("…which adds a file", wait(lambda: ctx_item(a, lambda i: i["ref"] == readme)) is not None, outer_cap())

    tracked = os.path.join(active()["cwd"], "README.md")  # tracked in the worktree
    open(tracked, "a").write("added line\n")
    readme = os.path.realpath(tracked)
    tm("select-pane", "-t", sb)
    keys(sb, "d")
    wait(lambda: "a add hunk" in outer_cap())
    for _ in range(6):
        tmux_outer("send-keys", "-t", "o", "j")
    tmux_outer("send-keys", "-t", "o", "a")
    check("a in the diff view adds the hunk's lines", wait(lambda: ctx_item(a, lambda i: i["kind"] == "lines" and i["ref"] == readme)) is not None, outer_cap())
    tmux_outer("send-keys", "-t", "o", "q")

def tmux_outer(*a):
    return run("tmux", "-L", server + "-outer", *a)

def outer_cap():
    return tmux_outer("capture-pane", "-p", "-t", "o")

def cleanup():
    subprocess.run(["tmux", "-L", server + "-outer", "kill-server"], env=env, capture_output=True)
    subprocess.run(["tmux", "-L", server, "kill-server"], env=env, capture_output=True)
    if not VERBOSE:
        shutil.rmtree(tmp, ignore_errors=True)
    else:
        print("kept", tmp)

try:
    main()
except Exception as e:  # noqa: BLE001
    failed.append(f"crashed: {e!r}")
    print("CRASH", repr(e))
finally:
    cleanup()
print(f"\n{passed} passed, {len(failed)} failed")
sys.exit(1 if failed else 0)
