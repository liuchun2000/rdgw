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

### Recommended split listeners

Split mode keeps the public client connection endpoint separate from the
internal HTTP/WebSocket proxy endpoint. The `connect-listen` address should be
reachable by the client or by the TLS-terminating reverse proxy. The
`proxy-listen` and `socks5-listen` addresses must be restricted to trusted
internal callers with firewall rules, security groups, or NetworkPolicy.

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

### TCP and SSH through SOCKS5

The SOCKS5 listener accepts TCP `CONNECT` requests. The username identifies the
remotedialer client. The password must be non-empty for the current protocol
implementation, but is not a separate business authorization mechanism; keep
the listener on a trusted internal network.

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
are separate from ordinary client tokens.

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

### Current limitations

- The example client currently requires `-token-file`; older client commands
  without a token file must be migrated when server client authentication is
  enabled.
- If `-client-auth-file` is omitted, the example server accepts any non-empty
  `X-Tunnel-ID`. Treat this as an unsafe compatibility mode, never as a public
  deployment configuration.
- In single mode, `/client/...` shares the listener with `/connect`; network
  isolation is required to prevent direct public access to the proxy route.
- The proxy forwards individual HTTP/HTTPS/WS/WSS requests. It does not rewrite
  HTML, JavaScript, CSS, redirects, cookies, or absolute resource URLs, so it is
  not a transparent whole-site reverse proxy.
- Complete IPv6 authority handling, client outbound allowlists, full timeout and
  concurrency limits, CLI private-CA options for client/peer, and full TLS
  integration tests remain follow-up work.
- Do not enable `-debug` in production. It enables tunnel data debugging and can
  expose sensitive payloads in logs.

# Versioning

See [VERSION.md](VERSION.md).
