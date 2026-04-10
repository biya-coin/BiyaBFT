#!/bin/bash
# start_4nodes.sh - BiyaBFT 四节点网络部署脚本
# 用法: bash start_4nodes.sh
set -e

pkill test_node

echo "=== 1. 编译节点 ==="
go build -tags bls12381 -o test_node main.go sample_app.go
make supernova 

echo "=== 2. 初始化节点数据与配置 ==="
rm -rf .supernova/node*

# 基础端口设置
BASE_P2P=26656
BASE_RPC=26657
BASE_UDP=12000
BASE_TCP=13000
BASE_QUIC=13000
STEP=10

for i in $(seq 0 3); do
  DIR=".supernova/node$i"
  mkdir -p "$DIR"/{config,data,log}
  # 参考 biyachain: biyachaind init $MONIKER --chain-id $CHAINID --home $BIYAHOME
  # init 命令会自动创建 config/config.toml、config/genesis.json 等文件
  # 必须使用 --home 参数指定绝对路径，否则 RootDir 会默认使用 $HOME/.supernova
  ABS_DIR="$(pwd)/$DIR"
  bin/supernova init --key-type bls12_381 --home "$ABS_DIR"
  sed -i 's/"ed25519"/"bls12_381"/g' "$DIR/config/genesis.json"
done

echo "=== 3. 收集并合成多验证者 Genesis ==="
VALIDATORS_JSON=""
for i in $(seq 0 3); do
  GENESIS_FILE=".supernova/node$i/config/genesis.json"
  VALIDATOR=$(python3 -c "
import json
with open('$GENESIS_FILE') as f:
    g = json.load(f)
v = g['validators'][0]
v['name'] = 'node$i'
print(json.dumps(v))
")
  if [ -z "$VALIDATORS_JSON" ]; then
    VALIDATORS_JSON="$VALIDATOR"
  else
    VALIDATORS_JSON="$VALIDATORS_JSON,$VALIDATOR"
  fi
done

python3 - <<PYEOF
import json
with open('.supernova/node0/config/genesis.json') as f:
    genesis = json.load(f)

validators = []
for i in range(4):
    with open(f'.supernova/node{i}/config/genesis.json') as f:
        g = json.load(f)
    v = g['validators'][0]
    v['name'] = f'node{i}'
    validators.append(v)
genesis['validators'] = validators
merged = json.dumps(genesis, indent=2)

for i in range(4):
    with open(f'.supernova/node{i}/config/genesis.json', 'w') as f:
        f.write(merged)
PYEOF

echo "=== 4. 获取各节点的 node-id 并组装 Persistent Peers ==="
NODE_IDS=()
PERSISTENT_PEERS=""

# 使用 comet show-node-id 命令获取 node-id（参考 biyachain: biyachaind comet show-node-id）
for i in $(seq 0 3); do
  DIR=".supernova/node$i"
  ABS_DIR="$(pwd)/$DIR"
  
  # 使用 comet 子命令，和 biyachain 的命令结构一致
  # 过滤掉 INFO 日志行和空行，只保留 node-id
  NODE_ID=$(bin/supernova comet show-node-id --home "$ABS_DIR" 2>&1 | grep -v "INFO" | grep -v "^$")
  
  if [ -z "$NODE_ID" ]; then
    echo "警告: 无法获取节点 $i 的 node-id"
    NODE_ID="unknown-node-$i"
  else
    echo "节点 $i 的 node-id: $NODE_ID"
  fi
  
  NODE_IDS+=("$NODE_ID")
done

# 构建 persistent_peers 字符串
for i in $(seq 0 3); do
  P2P_PORT=$((BASE_P2P + i * STEP))
  PEER="${NODE_IDS[$i]}@127.0.0.1:${P2P_PORT}"
  if [ -z "$PERSISTENT_PEERS" ]; then
    PERSISTENT_PEERS="$PEER"
  else
    PERSISTENT_PEERS="$PERSISTENT_PEERS,$PEER"
  fi
done

echo "Persistent Peers: $PERSISTENT_PEERS"

echo "=== 5. 配置各节点的端口和 persistent_peers ==="
for i in $(seq 0 3); do
  DIR=".supernova/node$i"
  CFG="$DIR/config/config.toml"
  
  P2P_PORT=$((BASE_P2P + i * STEP))
  RPC_PORT=$((BASE_RPC + i * STEP))
  UDP_PORT=$((BASE_UDP + i))
  TCP_PORT=$((BASE_TCP + i))
  QUIC_PORT=$((BASE_QUIC + i))
  
  echo "配置节点 $i 的端口: P2P=$P2P_PORT, RPC=$RPC_PORT, UDP=$UDP_PORT, TCP=$TCP_PORT, QUIC=$QUIC_PORT"
  
  # 配置 P2P 端口
  sed -i "s|laddr = \"tcp://0.0.0.0:26656\"|laddr = \"tcp://0.0.0.0:$P2P_PORT\"|" "$CFG"
  
  # 配置 RPC 端口
  sed -i "s|laddr = \"tcp://127.0.0.1:26657\"|laddr = \"tcp://127.0.0.1:$RPC_PORT\"|" "$CFG"
  sed -i "s|laddr = \"tcp://0.0.0.0:26657\"|laddr = \"tcp://0.0.0.0:$RPC_PORT\"|" "$CFG"
  
  # 配置 persistent_peers
  sed -i "s/^persistent_peers = .*/persistent_peers = \"$PERSISTENT_PEERS\"/" "$CFG"
  
  # 配置 libp2p 端口（删除已有配置，然后在 [p2p] 区块后追加）
  sed -i '/^quic_port = /d; /^tcp_port = /d; /^udp_port = /d' "$CFG"
  
  # 使用 Python 在 [p2p] 区块后插入端口配置
  python3 -c "
import re
content = open('$CFG').read()
port_block = '''\nquic_port = $QUIC_PORT\ntcp_port = $TCP_PORT\nudp_port = $UDP_PORT\n'''
content = re.sub(r'(\[p2p\][^\[]*)', lambda m: m.group(1).rstrip() + port_block, content, count=1)
open('$CFG', 'w').write(content)
"
done

echo "=== 6. 启动多节点 (后台运行) ==="

# BiyaBFT 使用 libp2p，需要通过 SUPERNOVA_BOOTSTRAP_ENRS 环境变量或 config.P2P.Seeds 配置 bootstrap 节点
# 先启动 node0 作为 bootstrap 节点
DIR0=".supernova/node0"
LOG0="$DIR0/log/node.log"
echo "启动节点 0 (bootstrap) → 日志: $LOG0"
./test_node --cmt-home "$DIR0" > "$LOG0" 2>&1 &

# 等待 node0 的 libp2p multiaddr 就绪
sleep 2
NODE0_MULTIADDR=$(grep -oP '/ip4/[0-9.]+/(tcp|udp|quic)/[0-9]+/p2p/[a-zA-Z0-9]+' "$LOG0" 2>/dev/null | head -1)
if [ -n "$NODE0_MULTIADDR" ]; then
    # 将 0.0.0.0 替换为 127.0.0.1
    NODE0_MULTIADDR=$(echo "$NODE0_MULTIADDR" | sed 's|/ip4/0.0.0.0/|/ip4/127.0.0.1/|')
    echo "Node0 libp2p 地址: $NODE0_MULTIADDR"
else
    echo "警告: 未能提取 node0 的 libp2p 地址，其他节点可能无法连接"
fi

# 启动节点 1-3，使用 node0 作为 bootstrap
for i in $(seq 1 3); do
  DIR=".supernova/node$i"
  LOG="$DIR/log/node.log"
  echo "启动节点 $i → 日志: $LOG"
  
  if [ -n "$NODE0_MULTIADDR" ]; then
    SUPERNOVA_BOOTSTRAP_ENRS="$NODE0_MULTIADDR" \
    ./test_node --cmt-home "$DIR" > "$LOG" 2>&1 &
  else
    ./test_node --cmt-home "$DIR" > "$LOG" 2>&1 &
  fi
  sleep 0.5
done

echo "启动完成！日志分别在 .supernova/node0~3/log/node.log"
echo ""
echo "提示:"
echo "  - 查看节点 N 的日志: tail -f .supernova/nodeN/log/node.log"
echo "  - 停止所有节点: pkill test_node"
echo ""
