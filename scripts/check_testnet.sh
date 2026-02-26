#!/bin/bash

# Check script to verify multi-node testnet status
# Usage: Run from the project root: bash scripts/check_testnet.sh

# Resolve project root (parent of scripts/)
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(dirname "$SCRIPT_DIR")"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

echo -e "${BLUE}========================================${NC}"
echo -e "${BLUE}  SupernovaCore Testnet Status${NC}"
echo -e "${BLUE}========================================${NC}"
echo ""

# Function to check node status
check_node() {
    local NODE_NUM=$1
    local PORT=$2
    local RPC_URL="http://127.0.0.1:${PORT}"

    echo -e "${YELLOW}Checking Node ${NODE_NUM} (RPC: ${PORT})...${NC}"

    # Check if process is running
    if pgrep -f "mytestnet/node${NODE_NUM}" > /dev/null; then
        echo -e "  ${GREEN}✓ Process is running${NC}"
    else
        echo -e "  ${RED}✗ Process is NOT running${NC}"
        return 1
    fi

    # Check RPC endpoint
    STATUS=$(curl -s "${RPC_URL}/status" 2>/dev/null)
    if [ $? -eq 0 ] && [ -n "$STATUS" ]; then
        echo -e "  ${GREEN}✓ RPC is responding${NC}"

        # Extract and display useful info
        LATEST_HEIGHT=$(echo $STATUS | grep -o '"latest_block_height":"[^"]*"' | cut -d'"' -f4)
        if [ -n "$LATEST_HEIGHT" ]; then
            echo -e "  Latest Block Height: ${GREEN}${LATEST_HEIGHT}${NC}"
        fi
    else
        echo -e "  ${RED}✗ RPC is NOT responding${NC}"
    fi

    # Check log file for errors
    LOG_FILE="$PROJECT_ROOT/mytestnet/logs/node${NODE_NUM}.log"
    if [ -f "$LOG_FILE" ]; then
        ERROR_COUNT=$(grep -i "error\|panic\|fatal" "$LOG_FILE" 2>/dev/null | wc -l | tr -d ' ')
        if [ "$ERROR_COUNT" -gt 0 ]; then
            echo -e "  ${RED}⚠ Found ${ERROR_COUNT} error(s) in log${NC}"
        else
            echo -e "  ${GREEN}✓ No errors in log${NC}"
        fi
    fi

    echo ""
}

# Function to check network connectivity
check_network() {
    echo -e "${YELLOW}Checking Network Connectivity...${NC}"

    # Count connected peers
    for i in {0..3}; do
        PORT=$((26657 + i))
        NET_INFO=$(curl -s "http://127.0.0.1:${PORT}/net_info" 2>/dev/null)
        if [ $? -eq 0 ]; then
            PEERS=$(echo $NET_INFO | grep -o '"n_peers":[0-9]*' | cut -d':' -f2)
            echo -e "  Node ${i}: ${GREEN}${PEERS} peers${NC}"
        fi
    done
    echo ""
}

# Function to check consensus
check_consensus() {
    echo -e "${YELLOW}Checking Consensus Status...${NC}"

    for i in {0..3}; do
        PORT=$((26657 + i))
        NODE_URL="http://127.0.0.1:${PORT}"
        STATUS=$(curl -s "${NODE_URL}/status" 2>/dev/null)

        if [ $? -eq 0 ]; then
            # Extract consensus info
            CATCHING_UP=$(echo $STATUS | grep -o '"syncing":[a-z]*' | cut -d':' -f2)
            LATEST_HEIGHT=$(echo $STATUS | grep -o '"latest_block_height":"[^"]*"' | cut -d'"' -f4)

            if [ "$CATCHING_UP" = "true" ]; then
                echo -e "  Node ${i}: ${YELLOW}Syncing${NC} (Height: ${LATEST_HEIGHT})"
            else
                echo -e "  Node ${i}: ${GREEN}Caught up${NC} (Height: ${LATEST_HEIGHT})"
            fi
        fi
    done
    echo ""
}

# Function to show recent blocks
show_recent_blocks() {
    echo -e "${YELLOW}Recent Blocks (from Node 0)...${NC}"

    BLOCKS=$(curl -s "http://127.0.0.1:26657/blockchain" 2>/dev/null)
    if [ $? -eq 0 ]; then
        LAST_HEIGHT=$(echo $BLOCKS | grep -o '"last_height":"[^"]*"' | cut -d'"' -f4)
        echo -e "  Last Height: ${GREEN}${LAST_HEIGHT}${NC}"
    fi
    echo ""
}

# Main execution
echo ""

# Check each node
for i in {0..3}; do
    PORT=$((26657 + i))
    check_node $i $PORT
done

# Check network and consensus
check_network
check_consensus
show_recent_blocks

echo -e "${BLUE}========================================${NC}"
echo -e "${BLUE}To view logs:${NC}"
echo -e "  tail -f mytestnet/logs/node0.log"
echo -e "  tail -f mytestnet/logs/node1.log"
echo -e "  tail -f mytestnet/logs/node2.log"
echo -e "  tail -f mytestnet/logs/node3.log"
echo -e "${BLUE}========================================${NC}"
