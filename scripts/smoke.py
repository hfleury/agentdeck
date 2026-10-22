#!/usr/bin/env python3
"""Smoke test with real Claude (Haiku, four tiny prompts). Isolated tmux
server and state; uses your normal Claude login. Transcripts it creates are
removed afterwards.

    python3 scripts/smoke.py
"""
import json, os, shutil, subprocess, sys, tempfile, time

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
BIN = os.path.join(ROOT, "bin", "agentdeck")
tmp = tempfile.mkdtemp(prefix="agentdeck-smoke-")
server = f"agentdeck-smoke-{os.getpid()}"
env = {k: v for k, v in os.environ.items()
       if k not in ("TMUX", "TMUX_PANE", "CLAUDECODE") and not k.startswith("CLAUDE_CODE_")}
env.update({"AGENTDECK_TMUX": server, "AGENTDECK_STATE_DIR": os.path.join(tmp, "state"),
            "AGENTDECK_SOCK": os.path.join(tmp, "sock"), "AGENTDECK_NOTIFY": "off",
            "AGENTDECK_NO_ATTACH": "1"})
passed, failed = 0, []

def check(name, ok, detail=""):
    global passed
    if ok:
        passed += 1; print(f"  ok   {name}")
    else:
        failed.append(name); print(f"  FAIL {name}\n{detail}")

def run(*a):
    return subprocess.run(a, env=env, capture_output=True, text=True, timeout=30).stdout

def tm(*a): return run("tmux", "-L", server, *a)
def cap(p): return tm("capture-pane", "-p", "-J", "-t", p, "-S", "-200")
def lines(p, n):
    """Last n non-blank screen lines."""
    return "\n".join([l for l in cap(p).splitlines() if l.strip()][-n:])
def state(): return json.loads(run(BIN, "state"))
def active():
    st = state(); return next(s for s in st["sessions"] if s["id"] == st["active"])
def wait(fn, t=60):
    end = time.time() + t
    while time.time() < end:
        try:
            r = fn()
            if r: return r
        except Exception: pass
        time.sleep(0.3)
    return None

repo = os.path.join(tmp, "repo")
os.makedirs(repo)
subprocess.run(["git", "-C", repo, "init", "-q"], check=True)
open(os.path.join(repo, "notes.txt"), "w").write("the secret word is MARMALADE\n")
created = set()
try:
    run(BIN)
    sid = run(BIN, "new", repo, "-m", "haiku").strip()
    pane = active()["pane"]
    # Folder trust prompt for the new folder.
    if wait(lambda: "trust this folder" in cap(pane).lower(), 20):
        check("folder trust prompt shows as needs input", wait(lambda: active()["state"] == "input", 5), active()["state"])
        print("  (answering the folder trust prompt: Yes)")
        tm("send-keys", "-t", pane, "Down")
        time.sleep(0.3)
        tm("send-keys", "-t", pane, "Enter")
    check("SessionStart hook arrives from real Claude", wait(lambda: active()["state"] in ("idle", "done"), 30), cap(pane))
    time.sleep(2)

    tm("set-buffer", "-b", "s", "/model ahsjnda\n")
    tm("paste-buffer", "-p", "-b", "s", "-t", pane)
    time.sleep(0.5)
    tm("send-keys", "-t", pane, "Enter")
    time.sleep(4)
    screen = cap(pane)
    print("----- claude after pasted /model ahsjnda -----\n" + lines(pane, 8) + "\n-----")
    check("pasted /model ahsjnda ran as a command (Claude mentions the model)", "ahsjnda" in screen.lower(), "")
    check("…and was not sent as a message (no turn started)", active()["state"] != "working")

    tm("send-keys", "-t", pane, "-l", "Read @notes.txt and reply with only the secret word")
    time.sleep(0.5)
    time.sleep(0.5)
    tm("send-keys", "-t", pane, "Enter")
    wait(lambda: active()["state"] in ("working", "tool"), 20)
    check("typed prompt → working → done", wait(lambda: active()["state"] == "done" and active()["title"].startswith("Read @notes"), 90), cap(pane))
    check("Claude answered from the @file", "MARMALADE" in cap(pane), cap(pane)[-1500:])
    created.add(active()["claude_id"])

    tm("set-buffer", "-b", "s", "Count the lines of this message and reply with only the number.\nsecond\nthird")
    tm("paste-buffer", "-p", "-b", "s", "-t", pane)
    time.sleep(0.5)
    tm("send-keys", "-t", pane, "Enter")
    wait(lambda: active()["state"] in ("working", "tool"), 20)
    check("multi-line paste is one turn", wait(lambda: active()["state"] == "done" and active()["title"].startswith("Count the lines"), 90), cap(pane))
    tail = lines(pane, 10)
    print("----- claude after multi-line paste -----\n" + tail + "\n-----")
    check("all three lines arrived in one prompt", "Count the lines" in tail and "second" in tail and "third" in tail and tail.count("❯ Count") == 1, tail)
    hist = json.loads(subprocess.run([BIN, "state"], env=env, capture_output=True, text=True).stdout or "{}")
    db = __import__("sqlite3").connect(os.path.join(env["AGENTDECK_STATE_DIR"], "state.db"))
    hist = [r[0] for r in db.execute("SELECT text FROM history")]
    check("prompts typed in Claude's box are in the deck's history", any(h.startswith("Read @notes.txt") for h in hist) and any(h.startswith("Count the lines") for h in hist), str(hist))
    print("phase 2: context")
    run(BIN, "note", "The secret code for this project is PAPRIKA.")
    tm("send-keys", "-t", pane, "-l", "What is the secret code? Reply with only the code.")
    time.sleep(0.5)
    tm("send-keys", "-t", pane, "Enter")
    wait(lambda: active()["state"] in ("working", "tool"), 20)
    check("Claude answers from an attached note", wait(lambda: active()["state"] == "done", 90) and "PAPRIKA" in lines(pane, 12), lines(pane, 12))
    lab = os.path.join(tmp, "lab")
    os.makedirs(lab)
    open(os.path.join(lab, "lab.txt"), "w").write("the lab word is OREGANO\n")
    run(BIN, "add", lab)
    granted = wait(lambda: "as a working directory" in cap(pane), 15)
    time.sleep(3)
    print("----- after the deck typed /add-dir -----\n" + lines(pane, 8) + "\n-----")
    check("the deck granted the outside folder (/add-dir, question answered)", granted)
    tm("send-keys", "-t", pane, "-l", "Read lab.txt in the lab folder I attached and reply with only the lab word.")
    time.sleep(0.5)
    tm("send-keys", "-t", pane, "Enter")
    wait(lambda: active()["state"] in ("working", "tool", "input"), 20)
    done = wait(lambda: active()["state"] in ("done", "input"), 120)
    print("----- after asking for lab.txt -----\n" + lines(pane, 10) + "\n-----")
    check("Claude read a file in the granted folder (no permission prompt)", done and active()["state"] == "done" and "OREGANO" in lines(pane, 12))
    calls = json.loads(subprocess.run([BIN, "state"], env=env, capture_output=True, text=True).stdout or "{}")
    db = __import__("sqlite3").connect(os.path.join(env["AGENTDECK_STATE_DIR"], "state.db"))
    tools = db.execute("SELECT tool, status, substr(output,1,200) FROM events WHERE type='tool' ORDER BY id").fetchall()
    print("  tool calls:", tools)
    check("real tool calls are recorded with their output", any(t[1] == "ok" and "OREGANO" in (t[2] or "") for t in tools), str(tools))
    check("transcript saved (resumable)", wait(lambda: any(os.path.exists(os.path.join(os.path.expanduser("~/.claude/projects"), d, sid + ".jsonl")) for d in os.listdir(os.path.expanduser("~/.claude/projects"))), 5))
finally:
    tm("kill-server")
    proj = os.path.expanduser("~/.claude/projects")
    enc = "".join(c if c.isalnum() else "-" for c in os.path.realpath(repo))
    for d in os.listdir(proj):
        if d == enc or d.endswith(enc.lstrip("-")):
            shutil.rmtree(os.path.join(proj, d), ignore_errors=True)
    shutil.rmtree(tmp, ignore_errors=True)
print(f"\n{passed} passed, {len(failed)} failed")
sys.exit(1 if failed else 0)
