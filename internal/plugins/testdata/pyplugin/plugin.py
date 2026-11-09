# A plugin in another language, proving the protocol needs nothing from Go: one JSON
# object per line on stdin and stdout, standard library only. The test puts a shebang
# with this host's interpreter on the first line.
import json
import sys


def reply(request_id, result=None, error=None):
    message = {"jsonrpc": "2.0", "id": request_id}
    if error is not None:
        message["error"] = error
    else:
        message["result"] = result
    sys.stdout.write(json.dumps(message) + "\n")
    sys.stdout.flush()


for line in sys.stdin:
    request = json.loads(line)
    method = request.get("method")
    request_id = request.get("id")
    if method == "initialize":
        reply(request_id, {"protocol_version": 1, "plugin_id": "py-validator", "provides": {"hookActions": ["check"]}})
    elif method == "shutdown":
        reply(request_id, {})
    elif method == "hook.invoke":
        title = (request["params"].get("data") or {}).get("title", "")
        reply(request_id, {"allow": bool(title), "reason": "" if title else "python says no title"})
    elif request_id is not None:
        reply(request_id, error={"code": -32601, "message": "method not found"})
