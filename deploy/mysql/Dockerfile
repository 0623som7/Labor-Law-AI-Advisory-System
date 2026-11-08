# 为什么要把 my.cnf 打进镜像，而不是 bind mount 或 compose configs：
#
# MySQL 会静默忽略 world-writable 的配置文件（只打一条 Warning 就继续用默认值
# 启动）。Windows 上的 bind mount 一律落成 0777，compose 的 configs 在
# standalone 模式下也仍是 bind mount（实测仍是 0777），两者都会让整个 my.cnf
# 失效——参数看起来配了，@@ 查询全是默认值。
#
# 打进镜像后文件是 0644 root 所有，任何宿主机都不影响它，配置才真正生效。
FROM mysql:8.0

COPY my.cnf /etc/mysql/conf.d/my.cnf
RUN chmod 644 /etc/mysql/conf.d/my.cnf
