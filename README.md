# SuperNovaCore 

SupernovaCore is a Cosmos SDK v2 compatible consensus engine designed to be a drop in replacement for CometBFT v1. 
The following features are implemented in this version of SupernovaCore

1. August, 2024 version of HotStuff 1 latency consensus alogrithm (the original HotStuff-2 consensus
has been running on Meter mainnet with more than 300 physical committee validator ndoes for more than 4 years).

2. BLS signature aggregations for validator votes

3. Dedicated validator messaging subnet to improve the communication efficiency



## Build & Run

```bash
make supernova
./bin/supernova init
./bin/supernova start   # 或与 main 共用 -cmt-home 运行 ./main
```

**Bootstrap 节点（P2P 发现）**：不再写死，由配置或环境变量提供：
- 环境变量 `SUPERNOVA_BOOTSTRAP_ENRS`：逗号分隔的 ENR 或 multiaddr
- 或配置文件 `config.toml` 中 `[p2p] seeds`：逗号分隔的 ENR/multiaddr（与上同格式）

未配置时 bootstrap 列表为空，仅本机或已连上的节点参与发现。

**两节点互连（避免 total=0 peer）**：至少一个节点要把**对方**的 ENR 配进 seeds 或 `SUPERNOVA_BOOTSTRAP_ENRS`。建议步骤：1）先启动节点1，在日志中复制其输出的 `ENR=...`；2）在节点2 的 `config.toml` 的 `[p2p] seeds = "enr:...节点1的ENR..."` 或设置环境变量；3）启动节点2（会主动连节点1）。若希望节点1 也主动连节点2，再在节点1 的 seeds 中加上节点2 的 ENR 并重启节点1。两节点需使用同一份 `config/genesis.json`。

**同机多节点**：QUIC/TCP/UDP 端口改为**配置项**，不再根据 `laddr` 偏移计算。在 `config.toml` 的 `[p2p]` 中显式设置：节点1 可设 `quic_port = 13000`、`tcp_port = 13000`、`udp_port = 12000`；节点2 可设 `quic_port = 13001`、`tcp_port = 13001`、`udp_port = 12001`。未配置时使用默认 13000/13000/12000。ENR 公布的端口即为此配置，避免对端拨号端口不一致导致 connection refused。每个节点需使用不同的 P2P 端口；`laddr` 仍可设置（例如节点1 `tcp://0.0.0.0:26656`，节点2 `tcp://0.0.0.0:26657`），避免 “address already in use”。



rm -rf ~/.supernova/data/maindb.db/*
rm -rf ~/.supernova/badger/*
rm -rf ~/.supernova-node2/badger/*
rm -rf ~/.supernova-node2/data/maindb.db/*