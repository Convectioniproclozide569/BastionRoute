# BastionRoute (v0.1.0-alpha)

> **An outbound-initiated Layer-7 WebSocket orchestrator for WireGuard that enforces a strict zero-inbound port architecture.**

BastionRoute is an industrial-grade, anti-fragile, zero-trust Layer-3 overlay network designed to securely route WireGuard traffic over a stateful Layer-7 WebSocket transport.

By initiating all data pipelines via outbound-only connections, BastionRoute completely eliminates the need for inbound firewall rules or open port exposure. It transforms your network infrastructure into an entirely invisible fortress, blending your secure communications seamlessly into background web traffic.

---

## ⚡ Architectural Core

BastionRoute leverages a decoupled, multi-shim architecture that separates the data plane from the control plane to optimize transport efficiency and preserve cryptographic isolation:

* **Zero-Inbound Footprint:** The home gateway or target server establishes a persistent, outbound-initiated WebSocket control link to a stateless Cloud Relay. No ingress ports are ever opened on your local perimeter.
* **Double-Wrapper Encapsulation:** Layer-3 Noise-protocol frames (WireGuard UDP) are transparently ingested by a user-space Go shim, packed into Layer-7 WebSockets, and streamed over an encrypted TLS 1.3 pipeline.
* **Stateless Cloud Brokerage:** The public cloud relay functions as a zero-knowledge, blind broker. It routes traffic based entirely on atomic routing tags in memory, removing any persistent database or state synchronization requirements.
* **Anti-Fragile Lifecycle Supervision:** An integrated, infinite supervisor loop guarantees self-healing resilience. If a network socket collapses due to carrier routing switches, the shim cleanly drains user-space allocations, safeguards local interface continuity, and initiates an immediate redial sequence.

---

## 🚀 Performance Metrics (Real-World Test Data)

BastionRoute overcomes the traditional performance penalties associated with nested encapsulation (the "TCP-in-TCP Meltdown") to deliver line-speed throughput over volatile, high-latency wide-area cellular networks.

### Raw UDP Throughput Profile
When evaluated using raw UDP datagram tests over a high-latency (**165 ms**) mobile connection, the user-space channel queues achieve near-perfect efficiency:
* **Bitrate:** Rock-solid **80.0 Mbps** flatline consistency.
* **Jitter:** **0.145 ms**—enabling elite, real-time voice, video, and streaming transit.
* **Packet Loss:** **< 0.7%** under sustained saturation.

### Stateful TCP Consistency Profile
When running stateful TCP testing over the nested network overlay, the optimized frame-flushing engine prevents exponential backoffs:
* **Throughput:** Climbs linearly from an initial **45 Mbps** to a peak of **79.1 Mbps**.
* **Retransmissions:** **2** total drops across a sustained 10-second saturation test.
* **Congestion Stability:** Smooth window expansion up to **1.16 MBytes** without structural sawtooth collapses.

---

## 🛠️ Engine Configuration & Tuning Matrix

To achieve optimal performance across lossy or highly latent WAN links, the following operating system and network settings are natively utilized:

### 1. Loopback MTU Stabilization Matrix
To prevent catastrophic packet fragmentation at physical gateway boundaries, the underlying virtual WireGuard interface must be clamped to account for encapsulation overhead:

$$\text{Total Physical Ethernet MTU} = 1500 \text{ bytes}$$

$$\text{IPv4 Header (20B)} + \text{TCP Header (20B)} + \text{TLS/WS Overhead (32B)} + \text{WG Wrapper (56B)} = 128 \text{ bytes}$$

$$\text{Optimal Configured MTU} = 1500 - 128 = 1372 \longrightarrow 1360 \text{ bytes (Safe Baseline)}$$

### 2. Sockets & Congestion Control Tuning
On the host or public cloud relay machine, configure the Linux kernel to use Google's BBR (Bottleneck Bandwidth and RTT) congestion control algorithm rather than standard Cubic. BBR prevents packet loss from triggering premature window exhaustion:

```bash
# Enable BBR Congestion Control on the host system
sudo sysctl -w net.core.default_qdisc=fq
sudo sysctl -w net.ipv4.tcp_congestion_control=bbr
```
## 📦 Deployment Mechanics

### Prerequisites
* **Go 1.21+ compiler toolchain**
* `make` utility installed (standard on Linux/macOS)
* WireGuard installed and configured locally

### Installation & Compilation

BastionRoute utilizes a standard multi-binary `cmd/` architecture. The compilation step automatically leverages the `Makefile` to pull down required dependencies (including `github.com/gorilla/websocket`) and verify the Go environment. 

To download dependencies and compile all binaries into a localized execution folder simultaneously, run:

```
git clone https://github.com/klauscam/BastionRoute.git
cd BastionRoute
make
```

Once completed, both production-ready binaries will be available inside the local target execution directory:
* `bin/bastionroute-shim`
* `bin/bastionroute-relay`

To clean up build artifacts and purge compiled binaries from your workspace at any time, run:

```
make clean
```

---

## 🚀 Execution Guide

### 1. Running the Central Stateless Relay
Deploy the relay binary on a public-facing cloud server or localized DMZ boundary. This acts as the zero-knowledge broker mapping atomic routing tags in memory:

```
./bin/bastionroute-relay --port=443 --auth-token="your-secure-cluster-token"
```

### 2. Running the Server-Side Shim (Substation / Legacy Asset Gateway)
Execute the shim in server mode behind your restricted infrastructure to initiate the outbound-only TLS 1.3 WebSocket connection back to the public relay broker:

```
./bin/bastionroute-shim --wg-role=server --uri="wss://relay.yourdomain.com" --room="secure-room-id" --wg-ip="127.0.0.1" --wg-port=51820
```

### 3. Running the Client-Side User Pipeline (Engineering Workstation)
Execute the shim in client mode on your remote device. The internal user-space supervisor loop will automatically spin up a local interface to securely bridge your native WireGuard application:

```
./bin/bastionroute-shim --wg-role=client --uri="wss://relay.yourdomain.com" --room="secure-room-id" --peer-id="remote-peer-01" --wg-ip="127.0.0.1" --wg-port=51820
```

### OpenWrt
```bash
git clone [https://github.com/klauscam/bastionroute.git](https://github.com/klauscam/bastionroute.git)
cd bastionroute
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bastionroute-shim
```

### Termux (Android)
```bash
git clone [https://github.com/klauscam/bastionroute.git](https://github.com/klauscam/bastionroute.git)
cd bastionroute
GO_ENABLED=0 GOOS=android GOARCH=arm64 go build -o bastionroute-shim
```


### Running the Server-Side Control Plane
Run the shim in server mode behind your private infrastructure to establish the outbound control link to the public relay broker:

```
./bastionroute-shim --wg-role=server --uri="wss://relay.yourdomain.com" --room="secure-room-id" --wg-ip="127.0.0.1" --wg-port=51820
```

### Running the Client-Side User Pipeline
Run the shim in client mode on your remote device. The user-space supervisor loop will spawn a local UDP interface to bridge your native WireGuard application:

```
./bastionroute-shim --wg-role=client --uri="wss://relay.yourdomain.com" --room="secure-room-id" --peer-id="remote-peer-01" --wg-ip="127.0.0.1" --wg-port=51820
```

---

## 📋 Technical Blueprint Overview

| System Layer | Core Technical Mechanism | Security & Performance Objective |
| :--- | :--- | :--- |
| **Transport Wrapper** | Noise Protocol over TLS 1.3 WebSockets | Provides uninterrupted network access through common port TCP 443. |
| **Perimeter Hardening** | Outbound-Initiated Socket Brokerage | Eradicates public-facing IPv4/IPv6 target signatures. |
| **Control Keep-Alives** | Layer-7 Ping/Pong Heartbeat Interception | Bypasses restrictive carrier timeouts on silent lines. |
| **Resilience Supervisor** | Decoupled Socket Loop Recovery | Prevents local tunnel drops during physical WAN handoffs. |
| **Congestion Fix** | BBR Socket Optimization + Explicit Flushing | Resolves and eliminates TCP-in-TCP performance degradation. |

---

## 📄 License

This project is licensed under the Apache License 2.0 - see the [LICENSE](LICENSE) file for details.
