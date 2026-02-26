#!/bin/bash

# Multi-node testnet startup script for SupernovaCore
# Usage: Run from the project root: bash scripts/start_testnet.sh

# Resolve project root (parent of scripts/)
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(dirname "$SCRIPT_DIR")"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

echo -e "${GREEN}========================================${NC}"
echo -e "${GREEN}  SupernovaCore Multi-Node Testnet${NC}"
echo -e "${GREEN}========================================${NC}"
echo ""

# Check if main binary exists
if [ ! -f "$PROJECT_ROOT/main" ]; then
    echo -e "${RED}Error: main binary not found. Please run 'go build -o main' first.${NC}"
    exit 1
fi

# Create logs directory if it doesn't exist
mkdir -p "$PROJECT_ROOT/mytestnet/logs"

# Function to stop all nodes
stop_nodes() {
    echo -e "\n${YELLOW}Stopping all nodes...${NC}"
    pkill -f "mytestnet/node" || true
    sleep 2
    echo -e "${GREEN}All nodes stopped.${NC}"
}

# Trap SIGINT and SIGTERM to stop nodes
trap stop_nodes SIGINT SIGTERM

# Stop any existing nodes
echo -e "${YELLOW}Stopping any existing nodes...${NC}"
stop_nodes

# Start nodes
echo -e "${GREEN}Starting 4 nodes...${NC}"

# Node 0
echo -e "Starting Node 0 (P2P: 26656, RPC: 26657)..."
cd "$PROJECT_ROOT/mytestnet/node0"
../../main -cmt-home ./ > ../logs/node0.log 2>&1 &
NODE0_PID=$!
echo -e "  Node 0 PID: $NODE0_PID"
cd "$PROJECT_ROOT"

# Node 1
echo -e "Starting Node 1 (P2P: 26756, RPC: 26757)..."
cd "$PROJECT_ROOT/mytestnet/node1"
../../main -cmt-home ./ > ../logs/node1.log 2>&1 &
NODE1_PID=$!
echo -e "  Node 1 PID: $NODE1_PID"
cd "$PROJECT_ROOT"

# Node 2
echo -e "Starting Node 2 (P2P: 26856, RPC: 26857)..."
cd "$PROJECT_ROOT/mytestnet/node2"
../../main -cmt-home ./ > ../logs/node2.log 2>&1 &
NODE2_PID=$!
echo -e "  Node 2 PID: $NODE2_PID"
cd "$PROJECT_ROOT"

# Node 3
echo -e "Starting Node 3 (P2P: 26956, RPC: 26957)..."
cd "$PROJECT_ROOT/mytestnet/node3"
../../main -cmt-home ./ > ../logs/node3.log 2>&1 &
NODE3_PID=$!
echo -e "  Node 3 PID: $NODE3_PID"
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
echo -e "  Node 0: mytestnet/logs/node0.log"
echo -e "  Node 1: mytestnet/logs/node1.log"
echo -e "  Node 2: mytestnet/logs/node2.log"
echo -e "  Node 3: mytestnet/logs/node3.log"
echo ""
echo -e "RPC endpoints:"
echo -e "  Node 0: http://127.0.0.1:26657"
echo -e "  Node 1: http://127.0.0.1:26757"
echo -e "  Node 2: http://127.0.0.1:26857"
echo -e "  Node 3: http://127.0.0.1:26957"
echo ""
echo -e "${YELLOW}Press Ctrl+C to stop all nodes${NC}"
echo ""

# Wait for nodes to start
sleep 5

# Monitor logs
echo -e "${GREEN}Monitoring logs (Ctrl+C to stop)...${NC}"
echo ""

# Tail all logs
tail -f \
    "$PROJECT_ROOT/mytestnet/logs/node0.log" \
    "$PROJECT_ROOT/mytestnet/logs/node1.log" \
    "$PROJECT_ROOT/mytestnet/logs/node2.log" \
    "$PROJECT_ROOT/mytestnet/logs/node3.log"
