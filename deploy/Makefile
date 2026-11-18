# 部署薄壳：所有目标都转发给 ld.sh，零业务逻辑。
# 没有 make 的机器（Windows Git Bash）直接用：bash deploy/ld.sh <命令>

.PHONY: up down ps logs health preflight backup restore upgrade tag config

up:        ; bash deploy/ld.sh up
down:      ; bash deploy/ld.sh down
ps:        ; bash deploy/ld.sh ps
logs:      ; bash deploy/ld.sh logs $(SVC)
health:    ; bash deploy/ld.sh health
preflight: ; bash deploy/ld.sh preflight
config:    ; bash deploy/ld.sh config
backup:    ; bash deploy/ld.sh backup
restore:   ; bash deploy/ld.sh restore $(ARGS)
upgrade:   ; bash deploy/ld.sh upgrade $(TAG)
tag:       ; bash deploy/ld.sh tag $(TAG)
