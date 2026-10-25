Reverse Tunneling Dialer
========================

remotedialer creates a two-way connection between a server and a client, so
that a `net.Dial` can be performed on the server and actually connects to the
client running services accessible on localhost or behind a NAT or firewall.

Architecture
------------

### Abstractions

remotedialer consists of structs that organize and abstract a TCP connection
between a server and a client. Both client and server use ``Session``s, which
provides a means to make a network connection to a remote endpoint and keep
track of incoming connections. A server uses the ``Server`` object which
contains a ``sessionManager`` which governs one or more ``Session``s, while a
client creates a ``Session`` directly. A ``connection`` implements the
``io.Reader`` and ``io.WriteCloser`` interfaces, so it can be read from and
written to directly. The ``connection``'s internal ``readBuffer`` monitors the
size of the data it is carrying and uses ``backPressure`` to pause incoming
data transfer until the amount of data is below a threshold.

![](./docs/remotedialer.png)

### Data flow

![](./docs/remotedialer-flow.png)

A client establishes a session with a server using the server's URL. The server
upgrades the connection to a websocket connection which it uses to create a
``Session``. The client then also creates a ``Session`` with the websocket
connection.

The client sits in front of some kind of HTTP server, most often a kubernetes
API server, and acts as a reverse proxy for that HTTP resource. When a user
requests a resource from this remote resource, request first goes to the
remotedialer server. The application containing the server is responsible for
routing the request to the right client connection.

The request is sent through the websocket connection to the client and read
into the client's connection buffer. A pipe is created between the client and
the HTTP service which continually copies data between each socket. The request
is forwarded through the pipe to the remote service, draining the buffer. The
service's response is copied back to the client's buffer, and then forwarded
back to the server and copied into the server connection's own buffer. As the
user reads the response, the server's buffer is drained.

The pause/resume mechanism checks the size of the buffer for both the client
and server. If it is greater than the threshold, a ``PAUSE`` message is sent
back to the remote connection, as a suggestion not to send any more data. As
the buffer is drained, either by the pipe to the remote HTTP service or the
user's connection, the size is checked again. When it is lower than the
threshold, a ``RESUME`` message is sent, and the data transfer may continue.

### remotedialer in the Rancher ecosystem

remotedialer is used to connect Rancher to the downstream clusters it manages,
enabling a user agent to access the cluster through an endpoint on the Rancher
server. remotedialer is used in three main ways:

#### Agent config and tunnel server

When the agent starts, it initially makes a client connection to the endpoint
`/v3/connect/register`, which runs an authorizer that sets some initial data
about the node. The agent continues to connect to the endpoint `/v3/connect` on
a loop. On each connection, it runs an OnConnect handler which pulls down node
configuration data from `/v3/connect/config`.

#### Steve Aggregation

The steve aggregation server on the agent establishes a remotedialer Session
with Rancher, making the steve API on the downstream cluster accessible from
the Rancher server and facilitating resource watches.

#### Health Check

The clusterconnected controller in Rancher uses the established tunnel to check
that clusters are still responsive and sets alert conditions on the cluster
object if they are not.

#### HA operation (peering)

remotedialer supports a mode where multiple servers can be configured as peers.
In that mode all servers maintain a mapping of all remotedialer client connections
to all other servers, and can route incoming requests appropriately.

Therefore, http requests referring any of the remotedialer clients can be resolved
by any of the peer servers. This is useful for high availability, and Rancher
leverages that functionality to distribute downstream clusters (running agents
acting as remotedialer clients) among replica pods (acting as remotedialer
server peers). In case one Rancher replica pod breaks down, Rancher will
reassign its downstream clusters to others.

Peers authenticate to one another via a shared token.

Running Locally
---------------

The `server/` and `client/` binaries are standalone examples for controlled
testing and deployment experiments. The client must run on a host that can
reach the target services. The server must be reachable by the client.

### Compile

```shell
make server
make client
```

The client example accepts `-id` (default `foo`), `-connect` (default
`ws://localhost:8123/connect`), `-token-file` (required), and `-debug` (debug
log level, enabled by default). Its token file contains the client token; the
client sends it as `X-Tunnel-Token` together with `X-Tunnel-ID` during the
WebSocket handshake. The example process keeps running and reconnects after a
connection failure using cancellable exponential backoff with jitter; SIGINT
and SIGTERM stop the reconnect loop.

### Client authentication

The server authorization file maps each client ID to one or more active
tokens. Keep this file readable only by the server process.

```json
{
  "foo": [
    "foo-token-current",
    "foo-token-next"
  ],
  "bar": [
    "bar-token-current"
  ]
}
```

Create a token file for the client. The file contains only the token, optionally
followed by a newline.

```shell
mkdir -p conf
cat > conf/client-auth.json <<'EOF'
{
  "foo": [
    "foo-token-current",
    "foo-token-next"
  ]
}
EOF
printf '%s\n' 'foo-token-current' > conf/foo.token
chmod 600 conf/foo.token
chmod 600 conf/client-auth.json
```

### Library API notes

`ConnectToProxyWithDialer` is the client entry point for supplying a custom
local `Dialer`. The dialer is used when the remote server asks the client to
open a connection, so it can apply local routing, network namespaces, or other
connection policy. Pass `nil` to use the default `net.Dialer` behavior via
`ConnectToProxy`.

`Server.HasSession(clientKey)` reports whether a client session is currently
available. `Server.ListClients()` returns the currently known client keys,
including clients learned through peering. `Session.Dial(ctx, proto, address)`
opens a tunneled connection from an existing session and returns a
`net.Conn`.

The server-side `Server.ClientConnectAuthorizer` controls Connect requests
initiated by the client. It is deliberately unset by `New`, which means such
requests are denied by default. Applications that need client-initiated
connections must set this field explicitly and authorize the requested
protocol and address.

### Recommended split listeners

Split mode keeps the public client connection endpoint separate from the
internal HTTP/WebSocket proxy endpoint. The `connect-listen` address should be
reachable by the client or by the TLS-terminating reverse proxy. The
`proxy-listen` and `socks5-listen` addresses must be restricted to trusted
internal callers with firewall rules, security groups, or NetworkPolicy.
Split mode requires both `-connect-listen` and `-proxy-listen` and forbids
`-listen`; single mode uses `-listen` and forbids the split listener options.
The SOCKS5 listener is disabled unless `-socks5-listen` is explicitly set.
The `/client/...` HTTP proxy and SOCKS5 listener do not authenticate callers;
any caller that can reach them can use a connected client's network access.
Client tokens authenticate `/connect` sessions only. Protect these listeners
with the network boundary rather than exposing them as public APIs.

```shell
./server/server \
  -listener-mode split \
  -id first-server \
  -token peer-token \
  -client-auth-file ./conf/client-auth.json \
  -connect-listen 172.168.0.126:8123 \
  -proxy-listen 172.168.0.126:8124 \
  -socks5-listen 172.168.0.126:2222
```

```shell
./client/client \
  -id foo \
  -token-file ./conf/foo.token \
  -connect ws://172.168.0.126:8123/connect
```

### Origin allowlist

When browser requests send an `Origin` header to `/connect`, configure
`-origin-allowlist-file` with a JSON array of exact origins:

```json
["https://console.example.com", "https://admin.example.com"]
```

Any non-empty Origin not listed in the file is rejected. The file is optional;
when omitted, non-empty Origins are rejected. Connections without an Origin,
including the example client, remain supported. Origin matching is exact and
does not replace the client ID/token check.
The WebSocket upgrader also keeps Gorilla's default same-origin check, so an
allowlisted browser Origin may still need to match the server request Host.
When either auth file is configured, changes to configured files can be
reloaded together with `SIGHUP`; a failed reload leaves the previous snapshot
active.

When Nginx or an Ingress terminates TLS, use its public WSS address for the
client connection:

```shell
./client/client \
  -id foo \
  -token-file ./conf/foo.token \
  -connect wss://public.example.com/connect
```

The example server does not terminate TLS itself. Plain `ws://` is suitable only
for local or controlled-network testing. Production deployments should expose
only `/connect` through the TLS-terminating reverse proxy.

### Single-listener compatibility mode

Single mode preserves the legacy command shape, but `/connect` and `/client/...`
share one listener. Use it only when the listener is protected by a trusted
internal network boundary. Do not expose this port directly to the public
Internet.

`-listener-mode` defaults to `single`. Split mode requires both
`-connect-listen` and `-proxy-listen` and forbids `-listen`.

```shell
./server/server \
  -listener-mode single \
  -id first-server \
  -token peer-token \
  -client-auth-file ./conf/client-auth.json \
  -listen 172.168.0.126:8123 \
  -socks5-listen 172.168.0.126:2222
```

### HTTP, HTTPS, WebSocket, and WSS proxying

The proxy URL format is:

```text
http://SERVER:PROXY_PORT/client/CLIENT_ID/SCHEME/TARGET_HOST[:PORT]/TARGET_PATH
```

Supported schemes are `http`, `https`, `ws`, and `wss`. Explicit target ports
are supported and are preserved in the upstream HTTP `Host` header.

```shell
curl -v \
  'http://172.168.0.126:8124/client/foo/http/updownload.cogiot.net:5000/cogiot/jstt/'
```

```shell
curl -v \
  'http://172.168.0.126:8124/client/foo/https/www.baidu.com/'
```

For an upstream service requiring HTTP Basic Authentication, send the normal
`Authorization` header. The proxy forwards ordinary application headers.

```shell
curl -v \
  -u 'username:password' \
  'http://172.168.0.126:8124/client/foo/http/updownload.cogiot.net:5000/cogiot/jstt/'
```

WebSocket proxy requests must use a WebSocket client and a valid Upgrade
handshake. In the proxy path, the `ws` or `wss` segment selects the protocol
used from the client-side host to the target service. The URL scheme used by
the caller is separate: use `ws://` for an unencrypted server entry point, or
`wss://` when a TLS-terminating reverse proxy is in front of the server.

```text
ws://172.168.0.126:8124/client/foo/ws/internal.example.com/socket
wss://proxy.example.com/client/foo/wss/internal.example.com/socket
```

For upstream HTTPS/WSS services with private or self-signed certificates, keep
the default compatibility mode or explicitly enable certificate verification
with a CA bundle:

```shell
./server/server \
  -listener-mode split \
  -client-auth-file ./conf/client-auth.json \
  -connect-listen 172.168.0.126:8123 \
  -proxy-listen 172.168.0.126:8124 \
  -socks5-listen 172.168.0.126:2222 \
  -upstream-tls-verify \
  -upstream-ca-file ./conf/private-ca.pem
```

With `-upstream-tls-verify` omitted, upstream HTTPS/WSS certificate validation
is disabled for compatibility with internal self-signed services. This setting
must not be used as a substitute for securing the server-client connection.
When verification is enabled, `-upstream-ca-file` adds PEM certificates to the
system trust store. The ordinary example client has no private-CA flag for its
outbound WSS connection and uses the system trust store.

The example server does not impose an absolute lifetime on HTTP proxy requests.
After response headers arrive, it closes a response whose body has produced no
data for 60 seconds by default; configure this response-body idle timeout with
`-http-idle-timeout` (use `0` to disable). SSE streams with periodic data or
heartbeats can remain connected. This timeout does not limit request-body
uploads or total request duration. WebSocket/WSS upgrades are not subject to
this body idle timeout. Upstream response headers, including a WebSocket
handshake response, must arrive within 120 seconds by default; configure this
with `-response-header-timeout` (use `0` to disable).

HTTP/WS proxy concurrency defaults to 256 and SOCKS5 concurrency defaults to 32.
Configure them independently with `-http-proxy-limit` and `-socks5-limit`;
`0` means unlimited. These limits apply to active proxy connections, not to the
number of connected clients.

### TCP and SSH through SOCKS5

The SOCKS5 listener accepts TCP `CONNECT` requests. The username identifies the
remotedialer client and must match a currently connected client ID. The
password must be non-empty for protocol compatibility, but is not checked as a
credential and does not provide caller authorization. Each active SOCKS5
connection counts against `-socks5-limit`. Its 30-second handshake deadline
ends after CONNECT succeeds, so established TCP streams have no absolute
lifetime imposed by the example server. Keep the listener on a trusted internal
network.

```shell
ssh -o 'ProxyCommand=socat - SOCKS5:172.168.0.126:%h:%p,socksport=2222,socksuser=foo,sockspass=pass' root@10.0.0.20
```

```shell
ssh -o 'ProxyCommand=ncat --proxy 172.168.0.126:2222 --proxy-type socks5 --proxy-auth foo:pass %h %p' root@10.0.0.20
```

### Dummy HTTP service

Build and run the test HTTP service:

```shell
make dummy
mkdir -p /tmp/remotedialer-www
printf '%s\n' 'hello' > /tmp/remotedialer-www/index.html
cd /tmp/remotedialer-www
/path/to/dummy -listen :8125
```

Then request it through the proxy listener:

```shell
curl -v 'http://172.168.0.126:8124/client/foo/http/127.0.0.1:8125/'
```

### HA peering

Peers use the existing `-token` and `-peers id:token:url` options. Peer tokens
are separate from ordinary client tokens. Each server needs its own `-id` and
`-token`; configure the other server in `-peers` on both sides. Peer connections
use the system CA store by default; `-peer-ca-file` adds private CA
certificates for WSS peers and must be configured before peers are added. This
option does not affect ordinary client WSS connections or upstream HTTPS/WSS
trust settings.

Multiple peers are separated by commas, for example
`-peers first:token-a:ws://first/connect,second:token-b:ws://second/connect`.
The peer notification protocol encodes a client as `clientKey/sessionKey`;
therefore client keys must not contain `/` when peering is enabled.

```shell
./server/server \
  -listener-mode single \
  -id first \
  -token peer-token \
  -client-auth-file ./conf/client-auth.json \
  -listen 127.0.0.1:8123 \
  -peers second:peer-token:ws://127.0.0.1:8124/connect
```

```shell
./server/server \
  -listener-mode single \
  -id second \
  -token peer-token \
  -client-auth-file ./conf/client-auth.json \
  -listen 127.0.0.1:8124 \
  -peers first:peer-token:ws://127.0.0.1:8123/connect
```

```shell
./client/client \
  -id foo \
  -token-file ./conf/foo.token \
  -connect ws://127.0.0.1:8123/connect
```

For peer servers using WSS with certificates issued by a private CA, provide
that CA bundle on each server before adding the peer:

```shell
./server/server \
  -id first \
  -token peer-token \
  -peer-ca-file ./conf/peer-ca.pem \
  -listen 127.0.0.1:8123 \
  -peers second:peer-token:wss://peer-2.example.com/connect
```

### Metrics

The example server registers Prometheus session metrics at startup and exposes
them at `/metrics`. In split mode, `/metrics` is on the internal
`-proxy-listen` port; in single mode, it shares the `-listen` port because no
separate internal listener exists:

```shell
# single mode
curl -s http://127.0.0.1:8123/metrics | grep session_server
# split mode: use the -proxy-listen port
curl -s http://127.0.0.1:8124/metrics | grep session_server
```

Scrape it with any Prometheus-compatible collector. The main metric families
are:

> Note: Session metrics appear only after sessions or tunnel traffic have been
> observed. An idle server may expose only Go runtime and promhttp metrics.

- `session_server_total_add_websocket_session` /
  `total_remove_websocket_session`: client sessions added and removed,
  labeled by `clientkey` and `peer`.
- `session_server_total_add_connections` /
  `total_remove_connections`: tunneled connections opened and closed,
  labeled by `clientkey`, `proto`, and `addr`.
- `session_server_total_transmit_bytes` /
  `session_server_total_receive_bytes` /
  `session_server_total_transmit_error_bytes`: tunnel traffic volume per
  client.
- `session_server_total_peer_ws_attempt` /
  `total_peer_ws_connected` / `total_peer_ws_disconnected`: HA peer
  connection attempts and state changes, labeled by `peer`.

The endpoint is unauthenticated. In split mode it is intentionally restricted
to the internal proxy listener; in single mode, keep the listener on a trusted
network or protect it with a reverse proxy.

The library's metrics package also auto-registers these metrics during package
initialization when `CATTLE_PROMETHEUS_METRICS=true`. Otherwise metric updates
remain disabled until the application calls `metrics.Register()` or
`metrics.MustRegister(...)` explicitly.

### fakek8s example

`examples/fakek8s` is a small TCP echo client used for Kubernetes and tunnel
integration checks. It connects to `TARGET_HOST:TARGET_PORT`, sends an initial
message, and echoes received bytes. `TARGET_HOST` defaults to
`api-extension.cattle-system.svc.cluster.local`, `TARGET_PORT` to `6666`, and
`SEND_INTERVAL` to `5` seconds. The included
[`fakek8s.yaml`](examples/fakek8s/fakek8s.yaml) deploys it in the
`cattle-system` namespace; [`Dockerfile`](examples/fakek8s/Dockerfile) builds
the image.

### Current limitations

- The example client currently requires `-token-file`; older client commands
  without a token file must be migrated when server client authentication is
  enabled.
- If `-client-auth-file` is omitted, the example server accepts any non-empty
  `X-Tunnel-ID`; a token is not required by the server in this mode, although
  the example client still requires and sends a token file. Treat this as an
  unsafe compatibility mode, never as a public deployment configuration.
- In single mode, `/client/...` shares the listener with `/connect`; network
  isolation is required to prevent direct public access to the proxy route.
- The proxy forwards individual HTTP/HTTPS/WS/WSS requests. It does not rewrite
  HTML, JavaScript, CSS, redirects, cookies, or absolute resource URLs, so it is
  not a transparent whole-site reverse proxy.
- Each WebSocket data message and each tunnel read buffer is limited to 4 MiB;
  exceeding the limit closes the affected tunnel connection.
- Connect control messages are limited to 266 bytes. Oversized target addresses
  are rejected instead of being truncated.
- Peer TLS verification can be configured with `-peer-ca-file`; without it,
  peer certificates must chain to the system CA store. Client-side outbound
  target allowlists are intentionally not provided; client requests use the
  same network access as direct requests from the client host.
- The server does not originate WebSocket ping frames for client sessions. It
  relies on the client sending periodic ping frames; the example client does
  this automatically. Custom clients must implement the same heartbeat or the
  server will close an idle connection after 60 seconds.
- Do not enable `-debug` in production. It enables tunnel data debugging and can
  expose sensitive payloads in logs. The `CATTLE_TUNNEL_DATA_DEBUG=true`
  environment variable has the same effect. The client example's `-debug`
  option controls its log level; the server's `-debug` option enables
  `PrintTunnelData`.
- `/metrics` exposes Prometheus counters for sessions, peers, and tunnel traffic;
  in split mode it is available on `-proxy-listen`, while single mode shares
  `-listen`.

# Versioning

See [VERSION.md](VERSION.md).
