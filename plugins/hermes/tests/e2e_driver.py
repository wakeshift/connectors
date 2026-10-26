"""The plugin against a real Wakeshift (Wakeshift's own repository runs
this): stdin's first line is {"issuer", "session", "result"} (an app's
tool result as Hermes hands it to transform_tool_result); prints {"result"}
(what the model would see), then {"run": text} for each turn it starts, as
Hermes would run it. Stops when stdin closes."""
import json
import sys
import tempfile
import threading
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from wakeshift import plugin  # noqa: E402

out_lock = threading.Lock()


def say(v):
    with out_lock:
        sys.stdout.write(json.dumps(v) + "\n")
        sys.stdout.flush()


first = json.loads(sys.stdin.readline())
sid = first["session"]
p = None


def run(session, text, batch):
    say({"run": text, "session": session})

    def turn():  # the run's turn, as Hermes reports it
        p.turn_started(session_id=session, platform="api_server")
        p.turn_ended(session_id=session)
    threading.Thread(target=turn, daemon=True).start()
    return True


p = plugin.Wakeshift(state_dir=Path(tempfile.mkdtemp()), issuers=[first["issuer"]], wakers={"api_server": run},
                     listener_args={"poll_wait": 1, "backoff_max": 1})
p.turn_started(session_id=sid, platform="api_server")
res = p.transform(tool_name="mcp__app__watch", result=first["result"], session_id=sid, tool_call_id="call_1")
p.turn_ended(session_id=sid)
say({"result": res})
listener = p.sessions[sid].listener


def report():  # once it's listening: how
    while listener is not None and listener.state not in ("sse", "poll") and not listener.stopped:
        listener._stop.wait(0.05)
    say({"state": listener.state if listener else "none"})


threading.Thread(target=report, daemon=True).start()
sys.stdin.read()
p.stop()
