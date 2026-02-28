#!/bin/bash

# Multi-node testnet startup script for SupernovaCore.
# Automatically generates node configs if not yet initialized.
# Usage: Run from the project root: bash scripts/start_testnet.sh
#        Or via Makefile:           make testnet-start

# Resolve project root (parent of scripts/)
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(dirname "$SCRIPT_DIR")"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

echo -e "${GREEN}========================================${NC}"
echo -e "${GREEN}  SupernovaCore Multi-Node Testnet${NC}"
echo -e "${GREEN}========================================${NC}"
echo ""

# ── Check main binary ──────────────────────────────────────────────────────────
if [ ! -f "$PROJECT_ROOT/main" ]; then
    echo -e "${YELLOW}main binary not found, building with BLS12-381 support...${NC}"
    cd "$PROJECT_ROOT"
    go build -tags bls12381 -o main .
    if [ $? -ne 0 ]; then
        echo -e "${RED}Error: failed to build main binary.${NC}"
        exit 1
    fi
    echo -e "${GREEN}✓ main binary built successfully${NC}"
    echo ""
fi

# ── Auto-generate testnet configs if not yet initialized ───────────────────────
NODE0_CONFIG="$PROJECT_ROOT/mytestnet/node0/config/genesis.json"
if [ ! -f "$NODE0_CONFIG" ]; then
    echo -e "${YELLOW}Testnet not initialized. Generating node configs...${NC}"
    echo ""

    GEN_BIN="/tmp/gen_testnet_supernova"
    cd "$PROJECT_ROOT"

    # Build the generator (requires bls12381 tag)
    echo -e "  Building gen_testnet tool..."
    go build -tags bls12381 -o "$GEN_BIN" ./cmd/gen_testnet
    if [ $? -ne 0 ]; then
        echo -e "${RED}Error: failed to build gen_testnet tool.${NC}"
        exit 1
    fi

    # Run the generator
    echo -e "  Generating keys and configs for 4 nodes..."
    "$GEN_BIN" --out-dir "$PROJECT_ROOT/mytestnet" --nodes 4 --chain-id 100
    if [ $? -ne 0 ]; then
        echo -e "${RED}Error: gen_testnet failed.${NC}"
        exit 1
    fi

    echo ""
    echo -e "${GREEN}✓ Testnet configs generated successfully${NC}"
    echo ""
else
    echo -e "${GREEN}✓ Testnet already initialized (using existing configs)${NC}"
    echo -e "  (Run 'make testnet-reset' to regenerate from scratch)"
    echo ""
fi

# ── Create logs directory ──────────────────────────────────────────────────────
mkdir -p "$PROJECT_ROOT/mytestnet/logs"

# ── Function to stop all nodes ─────────────────────────────────────────────────
stop_nodes() {
    echo -e "\n${YELLOW}Stopping all nodes...${NC}"
    pkill -f "mytestnet/node" || true
    sleep 2
    echo -e "${GREEN}All nodes stopped.${NC}"
}

# Trap SIGINT and SIGTERM to stop nodes cleanly
trap stop_nodes SIGINT SIGTERM

# Stop any existing nodes
echo -e "${YELLOW}Stopping any previously running nodes...${NC}"
pkill -f "mytestnet/node" 2>/dev/null || true
sleep 1

# ── Start 4 nodes ──────────────────────────────────────────────────────────────
echo -e "${GREEN}Starting 4 nodes...${NC}"

# Node 0
echo -e "  Starting Node 0 (P2P: 26656, RPC: 26657)..."
cd "$PROJECT_ROOT/mytestnet/node0"
../../main -cmt-home ./ > ../logs/node0.log 2>&1 &
NODE0_PID=$!
cd "$PROJECT_ROOT"

# Node 1
echo -e "  Starting Node 1 (P2P: 26756, RPC: 26757)..."
cd "$PROJECT_ROOT/mytestnet/node1"
../../main -cmt-home ./ > ../logs/node1.log 2>&1 &
NODE1_PID=$!
cd "$PROJECT_ROOT"

# Node 2
echo -e "  Starting Node 2 (P2P: 26856, RPC: 26857)..."
cd "$PROJECT_ROOT/mytestnet/node2"
../../main -cmt-home ./ > ../logs/node2.log 2>&1 &
NODE2_PID=$!
cd "$PROJECT_ROOT"

# Node 3
echo -e "  Starting Node 3 (P2P: 26956, RPC: 26957)..."
cd "$PROJECT_ROOT/mytestnet/node3"
../../main -cmt-home ./ > ../logs/node3.log 2>&1 &
NODE3_PID=$!
cd "$PROJECT_ROOT"

echo ""
echo -e "${GREEN}========================================${NC}"
echo -e "${GREEN}All nodes started!${NC}"
echo -e "${GREEN}========================================${NC}"
echo ""
echo -e "Node PIDs:"
echo -e "  Node 0: $NODE0_PID"
echo -e "  Node 1: $NODE1_PID"
echo -e "  Node 2: $NODE2_PID"
echo -e "  Node 3: $NODE3_PID"
echo ""
echo -e "Log files:"
echo -e "  mytestnet/logs/node0.log"
echo -e "  mytestnet/logs/node1.log"
echo -e "  mytestnet/logs/node2.log"
echo -e "  mytestnet/logs/node3.log"
echo ""
echo -e "RPC endpoints:"
echo -e "  Node 0: http://127.0.0.1:26657"
echo -e "  Node 1: http://127.0.0.1:26757"
echo -e "  Node 2: http://127.0.0.1:26857"
echo -e "  Node 3: http://127.0.0.1:26957"
echo ""
echo -e "${YELLOW}Press Ctrl+C to stop all nodes${NC}"
echo ""

# Wait a moment for nodes to initialize
sleep 5

# Monitor logs
echo -e "${GREEN}Monitoring logs (Ctrl+C to stop)...${NC}"
echo ""

tail -f \
    "$PROJECT_ROOT/mytestnet/logs/node0.log" \
    "$PROJECT_ROOT/mytestnet/logs/node1.log" \
    "$PROJECT_ROOT/mytestnet/logs/node2.log" \
    "$PROJECT_ROOT/mytestnet/logs/node3.log"
