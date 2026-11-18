package db

import (
	"context"
	"errors"

	"github.com/redis/go-redis/v9"
)

// takeOnceScript 是「取出即删」的原子脚本。
//
// 为什么不用 RDB.GetDel：GETDEL 是 Redis **6.2** 才有的命令。旧版本（Windows 上
// 常见的 5.0.x 就是）会回 `ERR unknown command`，而这个错误在调用方通常被折叠成
// 「验证码不对 / 凭据无效」—— 于是所有人的人机验证、找回密码凭据全部失败，
// 日志里却只有一堆看起来像用户操作问题的 400，排查方向从一开始就是错的。
//
// EVAL 从 Redis 2.6 起就有，语义与 GETDEL 完全一致（单线程执行，GET+DEL 之间
// 不会插入别的命令），所以不存在「取到了但没删」或「删了但没拿到」的中间态。
var takeOnceScript = redis.NewScript(`
local v = redis.call('GET', KEYS[1])
if not v then
  return nil
end
redis.call('DEL', KEYS[1])
return v
`)

// TakeOnce 原子地取出并删除一个键，返回取到的值与是否命中。
//
// 返回 found=false 表示键不存在 / 已被消费 / 已过期 —— 对调用方是「这张票无效」，
// 而不是错误：一次性票据的正常失败路径就走这里。
// 返回 err 则代表 **Redis 本身不可用**，调用方必须按「服务暂不可用」处理，
// 绝不能折成「你填错了」——那会让基础设施故障伪装成用户输入问题。
func TakeOnce(ctx context.Context, key string) (value string, found bool, err error) {
	if RDB == nil {
		return "", false, redis.ErrClosed
	}
	raw, err := takeOnceScript.Run(ctx, RDB, []string{key}).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return "", false, nil
		}
		return "", false, err
	}
	s, ok := raw.(string)
	if !ok {
		// 脚本只会回 bulk string 或 nil，走到这里说明键里存的不是字符串
		// （被别的写入方当成了 hash/list）。按「取不到」处理而不是 panic：
		// 一次性票据的键空间写坏了，用户重新走一遍流程即可，不该让接口 500。
		return "", false, nil
	}
	return s, true, nil
}
