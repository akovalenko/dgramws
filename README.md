# What dgramws does

Dgramws connects to a websocket URL and sends a preconfigured UUID
value as a text message. The websocket server is supposed to be a
relay like
https://github.com/cisoakk/wsServer/tree/master/examples/relay or
https://git.int.sw4me.com/akovalenko/wsrelay , connecting predefined
pairs of clients authenticated by UUIDs.

Dgramws also binds one or more UDP sockets, assigning a virtual
channel number (0-65535) to each of them. Incoming UDP packets to this
socket are sent out on Websocket as a MessageType 2 (binary), prefixed
by a big-endian 16-bit virtual channel number. Incoming Websocket
packets, also prefixed with a virtual channel number, are sent out to
an UDP address specified by the `client` setting.

```json
{
  "relay":"wss://relay.example.com/relay-path",
  "uuid":"1e6479a4-5060-414a-a03e-bc083b4d93cb",
  "channels": [
    {"id": 1,
     "udp": {
       "bind": "127.0.0.1:4080",
       "client": "127.0.0.1:4081"
     }
    }
  ]
}
```

When `client` is present in the channel description, UDP datagrams
from its address are forwarded to websocket, and all other UDP
datagrams are discarded. When `client` is absent or empty, the source
host and port of a *first* datagram arriving to `bind` address is
remembered and all other, non-matching datagrams are filtered out.

# Why use dgramws

You most probably don't need it. I'm using it in an experiment,
forwarding Wireguard over websocket in the environment where UDP is
restricted.
