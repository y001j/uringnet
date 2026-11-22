# 压测服务端的定义，由 run.sh（本机）和 serve.sh（跨机器的服务端）共用。
# 用法：. scripts/servers.sh；server_cmd <名称> <可执行文件目录> 输出启动命令（不含监听地址）

ALL_SERVERS="stdcopy std epoll gnet rawloop uringnet uringnet-spin uringnet-zc uringnet-inc uringnet-best uringnet-best-zc"
SERVER_BINARIES="std stdcopy epoll gnet rawloop uringnet"

# uringnet 推荐配置：增量缓冲区 32 x 64KB（Linux 6.12+，旧内核自动退回）+ 自适应轮询
URINGNET_BEST="-inc -bufs 32 -bufsize 65536 -spin 50us"

server_cmd() {
  case $1 in
    uringnet)         echo "$2/uringnet" ;;
    uringnet-spin)    echo "$2/uringnet -spin 50us" ;;
    uringnet-zc)      echo "$2/uringnet -zc 16384" ;;
    uringnet-inc)     echo "$2/uringnet -inc -bufs 32 -bufsize 65536" ;;
    uringnet-best)    echo "$2/uringnet $URINGNET_BEST" ;;
    uringnet-best-zc) echo "$2/uringnet $URINGNET_BEST -zc 16384" ;;
    rawloop)          echo "env BUFS=256 $2/rawloop" ;;
    stdcopy|std|epoll|gnet) echo "$2/$1" ;;
    *) echo "unknown server: $1" >&2; return 1 ;;
  esac
}

# build_servers <可执行文件目录>：在 bench 目录下编译客户端、代理和所有服务端
build_servers() {
  mkdir -p "$1"
  go build -o "$1/client" ./client
  go build -o "$1/agent" ./agent
  for s in $SERVER_BINARIES; do go build -o "$1/$s" ./servers/$s; done
}
