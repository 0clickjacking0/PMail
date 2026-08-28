package hooks

import (
	"sync"
	"testing"

	"github.com/Jinnrry/pmail/dto/parsemail"
	"github.com/Jinnrry/pmail/hooks/framework"
	"github.com/Jinnrry/pmail/models"
	"github.com/Jinnrry/pmail/utils/context"
)

// fakeHook 实现 framework.EmailHook 接口，仅用于测试。
type fakeHook struct {
	name string
}

func (f *fakeHook) SendBefore(ctx *context.Context, email *parsemail.Email) {}
func (f *fakeHook) SendAfter(ctx *context.Context, email *parsemail.Email, err map[string]error) {
}
func (f *fakeHook) ReceiveParseBefore(ctx *context.Context, email *[]byte)         {}
func (f *fakeHook) ReceiveParseAfter(ctx *context.Context, email *parsemail.Email) {}
func (f *fakeHook) ReceiveSaveAfter(ctx *context.Context, email *parsemail.Email, ue []*models.UserEmail) {
}
func (f *fakeHook) GetName(ctx *context.Context) string { return f.name }
func (f *fakeHook) SettingsHtml(ctx *context.Context, url string, requestData string) string {
	return ""
}

func resetHookList() {
	HookList = map[string]framework.EmailHook{}
}

// TestRegisterAndGetHook 验证注册与按键读取使用同一个键。
// 这是历史 bug 的核心：注册用 GetName() 返回值（如 "SpamBlock"），
// 清理协程却用插件文件名（如 "spam_block"）去 delete，键不匹配导致
// 死插件永远残留在 HookList 中。
func TestRegisterAndGetHook(t *testing.T) {
	resetHookList()

	// 模拟真实流程：注册键 = GetName() 返回值。
	registeredKey := "SpamBlock"
	RegisterHook(registeredKey, &fakeHook{name: registeredKey})

	// 用同一个键能取到。
	if _, ok := GetHook(registeredKey); !ok {
		t.Fatalf("GetHook(%q) = not found, want found", registeredKey)
	}

	// 用插件文件名（不同键）取不到，说明键语义是正确的。
	if _, ok := GetHook("spam_block"); ok {
		t.Fatalf("GetHook(%q) = found, want not found (文件名不是注册键)", "spam_block")
	}

	// RemoveHook 用注册键移除后应取不到。
	RemoveHook(registeredKey)
	if _, ok := GetHook(registeredKey); ok {
		t.Fatalf("GetHook(%q) after RemoveHook = found, want not found", registeredKey)
	}
}

// TestAllHooksSnapshot 验证 AllHooks 返回的是当前快照。
func TestAllHooksSnapshot(t *testing.T) {
	resetHookList()

	RegisterHook("A", &fakeHook{name: "A"})
	RegisterHook("B", &fakeHook{name: "B"})

	snap := AllHooks()
	if len(snap) != 2 {
		t.Fatalf("AllHooks() len = %d, want 2", len(snap))
	}

	// 移除后快照不受影响（已拷贝），但新快照会变。
	RemoveHook("A")
	if len(snap) != 2 {
		t.Fatalf("snapshot should be immutable, got %d", len(snap))
	}
	if got := len(AllHooks()); got != 1 {
		t.Fatalf("AllHooks() after remove = %d, want 1", got)
	}
}

// TestHookNames 验证名称列表与注册键一致。
func TestHookNames(t *testing.T) {
	resetHookList()
	RegisterHook("SpamBlock", &fakeHook{name: "SpamBlock"})
	RegisterHook("WeChatPush", &fakeHook{name: "WeChatPush"})

	names := HookNames()
	if len(names) != 2 {
		t.Fatalf("HookNames() len = %d, want 2", len(names))
	}
	seen := map[string]bool{}
	for _, n := range names {
		seen[n] = true
	}
	if !seen["SpamBlock"] || !seen["WeChatPush"] {
		t.Fatalf("HookNames() = %v, want contain SpamBlock and WeChatPush", names)
	}
}

// TestConcurrentAccess 在 -race 下验证并发读写不会触发数据竞争。
// 模拟插件进程退出（RemoveHook）与收发邮件（AllHooks 遍历）同时进行。
func TestConcurrentAccess(t *testing.T) {
	resetHookList()
	const plugins = 50
	for i := 0; i < plugins; i++ {
		key := "hook" + string(rune('A'+i%26))
		RegisterHook(key, &fakeHook{name: key})
	}

	var wg sync.WaitGroup

	// 读侧：模拟收/发邮件时遍历插件。
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				for _, h := range AllHooks() {
					if h == nil {
						continue
					}
					_ = h.GetName(nil)
				}
			}
		}()
	}

	// 写侧：模拟插件进程退出时的移除。
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				key := "hook" + string(rune('A'+(i+j)%26))
				RemoveHook(key)
				RegisterHook(key, &fakeHook{name: key})
			}
		}(i)
	}

	wg.Wait()
}
