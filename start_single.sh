#!/bin/bash
# start_single.sh - BiyaBFT 单节点部署脚本
# 用法: bash start_single.sh
set -e

echo "=== 1. 编译节点 ==="
# make supernova
go build -tags bls12381 -o test_node main.go sample_app.go

echo "=== 2. 初始化节点配置文件 ==="
rm -rf .supernova
mkdir -p .supernova/{config,data}
cd .supernova && ../bin/supernova init --key-type bls12_381
sed -i 's/"ed25519"/"bls12_381"/g' config/genesis.json
cd ..

echo "=== 3. 启动节点 ==="
./test_node --cmt-home ./.supernova
