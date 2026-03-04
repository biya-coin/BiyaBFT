#!/usr/bin/env bash
# 多节点运行：使用内置 ProxyApp=kvstore（与 cmd/supernova/commands/node.go 中的 proxy_app 一致）
# 用法: ./scripts/run-multi-node-kvstore.sh [节点数，默认 3]
set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
DATA_DIR="${MULTI_KVSTORE_DIR:-$ROOT_DIR/data/multi-kvstore}"
BIN="$ROOT_DIR/bin/supernova"
NODES="${1:-3}"

if [[ ! -x "$BIN" ]]; then
  echo "Building supernova..."
  (cd "$ROOT_DIR" && make supernova)
fi

# 使用与 run-multi-node.sh 不同的端口，避免与 noop 多节点或 biyachain 多节点冲突
base_rpc=26757
base_quic=23000
base_udp=22000

# 可选：结束占用端口的旧 supernova 进程（仅限本脚本启动的 multi-kvstore 节点）
if [[ -f "$DATA_DIR/pids" ]]; then
  echo "Stopping previous multi-kvstore nodes..."
  while read -r pid; do kill "$pid" 2>/dev/null || true; done < "$DATA_DIR/pids" 2>/dev/null || true
  rm -f "$DATA_DIR/pids"
  sleep 2
fi

mkdir -p "$DATA_DIR"
cd "$ROOT_DIR"

# 1) 为每个节点创建目录并 init
for i in $(seq 1 "$NODES"); do
  home="$DATA_DIR/node$i"
  mkdir -p "$home/config" "$home/data"
  if [[ ! -f "$home/config/genesis.json" ]]; then
    (cd "$home" && "$BIN" init --home "$(pwd)")
  fi
done

# 2) 用 BLS 密钥和统一 genesis 覆盖（多验证者，需 -tags bls12381）
(cd "$ROOT_DIR" && go run -tags bls12381 scripts/gen-multi-node-keys.go "$DATA_DIR" "$NODES")

# 清除各节点旧 DB，避免 "genesis doc hash in db does not match loaded genesis doc"
for i in $(seq 1 "$NODES"); do
  rm -rf "$DATA_DIR/node$i/data"
  mkdir -p "$DATA_DIR/node$i/data"
  echo '{"height":"0","round":0,"step":0}' > "$DATA_DIR/node$i/data/priv_validator_state.json"
done

# 3) 各节点 config.toml：不同 RPC/P2P 端口，proxy_app=kvstore（内置应用，abci=socket）
for i in $(seq 1 "$NODES"); do
  home="$DATA_DIR/node$i"
  cfg="$home/config/config.toml"
  rpc=$((base_rpc + i - 1))
  quic=$((base_quic + i - 1))
  udp=$((base_udp + i - 1))

  sed -i.bak "s|^laddr = \"tcp://127.0.0.1:[0-9]*\"|laddr = \"tcp://127.0.0.1:$rpc\"|" "$cfg"
  sed -i.bak 's|^proxy_app = .*|proxy_app = "kvstore"|' "$cfg"
  sed -i.bak 's|^abci = .*|abci = "socket"|' "$cfg"

  if ! grep -q '^quic_port' "$cfg"; then
    sed -i.bak '/^\[p2p\]/a\
quic_port = '"$quic"'\
tcp_port = '"$quic"'\
udp_port = '"$udp"'
' "$cfg"
  else
    sed -i.bak "s/^quic_port = .*/quic_port = $quic/" "$cfg"
    sed -i.bak "s/^tcp_port = .*/tcp_port = $quic/" "$cfg"
    sed -i.bak "s/^udp_port = .*/udp_port = $udp/" "$cfg"
  fi
  rm -f "${cfg}.bak"
done

# 4) 启动节点 1，从日志获取 multiaddr 作为 node2..nodeN 的 seed
node1_home="$DATA_DIR/node1"
node1_log="$DATA_DIR/node1.log"
rm -f "$DATA_DIR/pids"
echo "Starting node 1 (proxy_app=kvstore, log: $node1_log)..."
"$BIN" start --home "$node1_home" > "$node1_log" 2>&1 &
NODE1_PID=$!
echo $NODE1_PID >> "$DATA_DIR/pids"
sleep 6
if ! kill -0 $NODE1_PID 2>/dev/null; then
  echo "Node 1 failed to start. Log:"
  tail -60 "$node1_log"
  exit 1
fi

# 从 node1 日志提取 multiAddr
SEED=""
for _ in $(seq 1 15); do
  SEED=$(grep -oE 'multiAddr=/ip4/[^ ]+/tcp/[0-9]+/p2p/[a-zA-Z0-9]+' "$node1_log" 2>/dev/null | head -1 | sed 's/^multiAddr=//')
  [[ -n "$SEED" ]] && break
  sleep 2
done

if [[ -n "$SEED" ]]; then
  echo "Node 1 multiaddr: $SEED"
  SEED="${SEED/0.0.0.0/127.0.0.1}"
  for i in $(seq 2 "$NODES"); do
    cfg="$DATA_DIR/node$i/config/config.toml"
    sed -i.bak "s|^seeds = .*|seeds = \"$SEED\"|" "$cfg"
    rm -f "${cfg}.bak"
  done
else
  echo "Warning: could not extract node1 multiaddr; other nodes may have 0 peers."
  echo "Check $node1_log for 'Node started p2p server' and set seeds in config manually."
fi

# 5) 启动其余节点
for i in $(seq 2 "$NODES"); do
  home="$DATA_DIR/node$i"
  log="$DATA_DIR/node$i.log"
  echo "Starting node $i (proxy_app=kvstore, log: $log)..."
  "$BIN" start --home "$home" > "$log" 2>&1 &
  echo $! >> "$DATA_DIR/pids"
done

echo ""
echo "Started $NODES node(s) with ProxyApp=kvstore. PIDs in $DATA_DIR/pids"
for i in $(seq 1 "$NODES"); do
  rpc=$((base_rpc + i - 1))
  quic=$((base_quic + i - 1))
  echo "  Node $i: RPC http://127.0.0.1:$rpc  P2P quic/tcp $quic udp $((base_udp + i - 1))"
done
echo "Logs: $DATA_DIR/node{N}.log"
echo "To stop: kill \$(cat $DATA_DIR/pids)"
echo ""
echo "Example (JSON-RPC):"
echo "  status:  curl -s -X POST http://127.0.0.1:$base_rpc -H 'Content-Type: application/json' -d '{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"status\"}'"
echo "  send tx: ./scripts/send-tx.sh http://127.0.0.1:$base_rpc 'key=value'"
