#!/bin/bash
# 修改或恢复tcp连接相关参数。请在压测前开启,压测后关闭。需要免密sudo

currDir=$(cd `dirname $0`; pwd)
tmpFileDir="/tmp/pitaya-high-bench"
confDir=${currDir}/kernalconf

#修改内核参数,为压测做准备
function modify() {
  #备份
  if [ -e ${tmpFileDir} ]; then
      echo "[ERROR]backup file exists!can't backup!"
      exit 1
  fi
  mkdir -p ${tmpFileDir}
  cat /proc/sys/net/ipv4/ip_local_port_range > ${tmpFileDir}/ip_local_port_range
  cat /proc/sys/net/ipv4/tcp_abort_on_overflow > ${tmpFileDir}/tcp_abort_on_overflow
  cat /proc/sys/net/ipv4/tcp_max_syn_backlog > ${tmpFileDir}/tcp_max_syn_backlog
  cat /proc/sys/net/core/somaxconn > ${tmpFileDir}/somaxconn
  cat /proc/sys/net/netfilter/nf_conntrack_max > ${tmpFileDir}/nf_conntrack_max
  #开始修改内核参数
#  ulimit -n 1000000
#ulimit只能对当前session生效,只能由用户自己手动设置了
  echo 1024 65535 > /proc/sys/net/ipv4/ip_local_port_range  # net.ipv4.ip_local_port_range
  echo 1 > /proc/sys/net/ipv4/tcp_abort_on_overflow
  echo 10000 >/proc/sys/net/ipv4/tcp_max_syn_backlog
  echo 10000 >/proc/sys/net/core/somaxconn
  echo 1000000 > /proc/sys/net/netfilter/nf_conntrack_max
}

#恢复内核参数,压测后调用
function restore() {
  if [[ ! -e ${tmpFileDir} ]]; then
      echo "[ERROR]backup file not exists!can't restore!"
      exit 1
  fi
  cat ${tmpFileDir}/ip_local_port_range > /proc/sys/net/ipv4/ip_local_port_range &&
  cat ${tmpFileDir}/tcp_abort_on_overflow > /proc/sys/net/ipv4/tcp_abort_on_overflow &&
  cat ${tmpFileDir}/tcp_max_syn_backlog > /proc/sys/net/ipv4/tcp_max_syn_backlog &&
  cat ${tmpFileDir}/somaxconn > /proc/sys/net/core/somaxconn &&
  cat ${tmpFileDir}/nf_conntrack_max > /proc/sys/net/netfilter/nf_conntrack_max &&
  rm -rf ${tmpFileDir}
}

# help
function fun_show_help(){
    printf 'usage: sudo ./kernalconf.sh <command>
command
   modify           修改内涵参数,为压测做准备
   restore          恢复内核参数,压测后调用
'
}

# main
if [[ "$1" = "modify" ]];then
  modify
elif [[ "$1" = "restore" ]];then
  restore
else
  fun_show_help
fi

