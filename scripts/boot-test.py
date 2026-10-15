# The host side of scripts/boot-test.sh, run in the container beside
# Firecracker: talks to the agent through Firecracker's vsock socket
# (/tmp/v.sock, "CONNECT <port>" then the stream) and plays the nodelet's end
# of the host channel on /tmp/v.sock_1025.
import base64, json, socket, subprocess, threading, time, http.server, os, sys

def vsock(port=1024, timeout=30):
    deadline = time.time() + timeout
    while True:
        try:
            s = socket.socket(socket.AF_UNIX)
            s.settimeout(timeout)
            s.connect("/tmp/v.sock")
            s.sendall(f"CONNECT {port}\n".encode())
            line = b""
            while not line.endswith(b"\n"):
                c = s.recv(1)
                if not c:
                    raise ConnectionError("closed during CONNECT")
                line += c
            if not line.startswith(b"OK"):
                raise ConnectionError(line)
            return s
        except (OSError, ConnectionError):
            if time.time() > deadline:
                raise
            time.sleep(0.1)

def agent(method, path, body=None, timeout=30):
    s = vsock(timeout=timeout)
    data = json.dumps(body).encode() if body is not None else b""
    s.sendall(f"{method} {path} HTTP/1.1\r\nHost: agent\r\nConnection: close\r\nContent-Length: {len(data)}\r\nContent-Type: application/json\r\n\r\n".encode() + data)
    resp = b""
    while True:
        c = s.recv(65536)
        if not c:
            break
        resp += c
    head, _, payload = resp.partition(b"\r\n\r\n")
    status = int(head.split()[1])
    if b"chunked" in head.lower():
        out, rest = b"", payload
        while rest:
            n, _, rest = rest.partition(b"\r\n")
            n = int(n, 16)
            if n == 0:
                break
            out, rest = out + rest[:n], rest[n + 2:]
        payload = out
    return status, payload


def recv_exact(s, n):
    b = b""
    while len(b) < n:
        c = s.recv(n - len(b))
        if not c:
            raise ConnectionError("closed")
        b += c
    return b

def ws_exec(query):
    """A minimal WebSocket client: exec, then read frames to the exit."""
    s = vsock()
    key = base64.b64encode(os.urandom(16)).decode()
    s.sendall(f"GET /exec?{query} HTTP/1.1\r\nHost: agent\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: {key}\r\nSec-WebSocket-Version: 13\r\n\r\n".encode())
    head = b""
    while not head.endswith(b"\r\n\r\n"):
        head += recv_exact(s, 1)
    assert b" 101 " in head.split(b"\r\n")[0], head
    frames = []
    while True:
        b0, b1 = recv_exact(s, 2)
        op, n = b0 & 0x0F, b1 & 0x7F
        if n == 126:
            n = int.from_bytes(recv_exact(s, 2), "big")
        elif n == 127:
            n = int.from_bytes(recv_exact(s, 8), "big")
        payload = recv_exact(s, n)
        if op == 8:
            return frames, int.from_bytes(payload[:2], "big") if len(payload) >= 2 else None
        frames.append((op, payload))

# The nodelet's end of the host channel: Firecracker hands a guest connection
# to vsock port 1025 to whoever listens on <uds>_1025.
class Host(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        b = json.dumps({"id": "sb-boottest", "name": "boottest", "path_seen": self.path}).encode()
        self.send_response(200); self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(b))); self.end_headers(); self.wfile.write(b)
    def log_message(self, *a): pass
class UnixHTTP(http.server.ThreadingHTTPServer):
    address_family = socket.AF_UNIX
    def server_bind(self):
        self.socket.bind(self.server_address)
    def get_request(self):
        c, _ = self.socket.accept()
        return c, ("vsock", 0)
srv = UnixHTTP("/tmp/v.sock_1025", Host)
threading.Thread(target=srv.serve_forever, daemon=True).start()

t0 = time.time()
st, _ = agent("POST", "/internal/wake", {"sandbox": "sb-boottest", "name": "boottest", "project": "test",
    "hostname": "boottest", "env": {"FOO": "bar"}, "user": "loam", "time": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())})
print(f"wake: {st} after {time.time()-t0:.2f}s", flush=True)
assert st == 204, st

def run(cmd, user=None):
    body = {"cmd": cmd}
    if user: body["user"] = user
    st, b = agent("POST", "/exec/run", body)
    assert st == 200, (st, b)
    r = json.loads(b)
    return r["exit_code"], base64.b64decode(r["stdout"]).decode(), base64.b64decode(r["stderr"]).decode()

code, out, err = run(["sh", "-c", "id; echo FOO=$FOO; hostname; uname -r; head -1 /etc/os-release; sudo -n id -u"])
print("exec/run as loam:", code, out, err, sep="\n", flush=True)
assert code == 0 and "uid=1000(loam)" in out and "FOO=bar" in out and "boottest" in out and out.strip().endswith("0"), out
code, out, _ = run(["id", "-u"], user="root")
assert code == 0 and out.strip() == "0", out

def ping(ip):
    return subprocess.run(["ping", "-c", "1", "-W", "2", ip], capture_output=True).returncode == 0

# eth0 came up from the kernel command line.
assert ping("10.240.0.7"), "no answer from 10.240.0.7 (loam.ip)"
# A wake carrying another address and MAC (a fork of a memory snapshot) moves it.
st, b = agent("POST", "/internal/wake", {"sandbox": "sb-boottest", "hostname": "boottest", "env": {"FOO": "bar"},
    "network": {"ip": "10.240.0.8/16", "gateway": "10.240.0.1", "dns": ["10.240.0.1"], "mac": "06:00:0a:f0:00:08"}})
assert st == 204, (st, b)
code, out, _ = run(["sh", "-c", "ip -4 -o addr show eth0; cat /sys/class/net/eth0/address; ip route show default; cat /etc/resolv.conf"])
print("network after a second wake:", out, sep="\n", flush=True)
assert "10.240.0.8/16" in out and "10.240.0.7" not in out and "06:00:0a:f0:00:08" in out and "default via 10.240.0.1" in out, out
assert ping("10.240.0.8"), "no answer from 10.240.0.8 after the wake"

st, b = agent("PUT", "/fs/file?path=/home/loam/hello.txt&mode=0600", None)
assert st == 200, (st, b)
st, b = agent("GET", "/fs/stat?path=/etc/hostname")
print("fs/stat:", st, b.decode(), flush=True)
assert st == 200 and json.loads(b)["type"] == "file"

code, out, err = run(["sh", "-c", "curl -fsS --unix-socket /run/loam/api.sock http://loam/self; echo; curl -fsS --unix-socket /run/loam/api.sock http://loam/tasks"])
print("guest socket:", code, out, err, sep="\n", flush=True)
assert code == 0 and "sb-boottest" in out and "/self" in out and "\"tasks\"" in out, (out, err)

frames, close = ws_exec("cmd=sh&cmd=-c&cmd=tty%3B+stty+size%3B+echo+%24TERM&tty=true&cols=120&rows=40")
first = json.loads(frames[0][1])
out = b"".join(p[1:] for op, p in frames if op == 2 and p[:1] == b"\x01").decode().replace("\r", "")
last = json.loads(frames[-1][1])
print("exec WebSocket (tty):", first, repr(out), last, "close", close, flush=True)
assert first["type"] == "started" and out.startswith("/dev/pts/") and "40 120" in out and "xterm-256color" in out
assert last == {"type": "exit", "code": 0} and close == 1000

st, b = agent("GET", "/internal/activity")
print("activity:", st, b.decode(), flush=True)
st, _ = agent("POST", "/internal/poweroff")
assert st == 204
print("poweroff: 204", flush=True)
