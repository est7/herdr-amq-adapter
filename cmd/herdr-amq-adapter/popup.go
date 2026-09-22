package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/est7/herdr-amq-adapter/internal/adapter"
	"github.com/est7/herdr-amq-adapter/internal/bridge"
)

type agentView struct {
	Handle, Pane, Server, Wake string
	Pending                    int
	Err                        string
}
type popupView struct {
	At     time.Time
	Agents []agentView
	Bridge bridgeView
	Errors []string
}

func inspectPopup(ctx context.Context, e env) popupView {
	v := popupView{At: time.Now()}
	recs, err := e.store.List()
	if err != nil {
		v.Errors = append(v.Errors, err.Error())
	}
	for _, r := range recs {
		a := agentView{Handle: r.Handle, Pane: r.PaneID, Server: r.ServerSocket, Wake: "parked"}
		if r.Generation != "" {
			st, err := adapter.WakeCheck(ctx, e.amq, e.root, r.Handle)
			if err != nil {
				a.Wake = "unavailable"
				a.Err = err.Error()
			} else {
				a.Wake = st.Status
			}
		}
		a.Pending, err = bridge.CountMessages(e.root + "/agents/" + r.Handle + "/inbox/new")
		if err != nil {
			a.Pending = -1
			a.Err = err.Error()
		}
		v.Agents = append(v.Agents, a)
	}
	snap, err := bridge.LoadSnapshot(e.configDir)
	if err != nil {
		v.Errors = append(v.Errors, err.Error())
	} else {
		v.Bridge = inspectBridge(ctx, e, snap)
	}
	return v
}

// Rendering consumes one observation; it never provisions or repairs anything.
func popupLines(v popupView) []string {
	lines := []string{"AMQ 通信状态", "观测时间 " + v.At.Format("15:04:05") + "  ·  每 5 秒刷新", "", "本机 agent · WAKE / 未读"}
	if len(v.Agents) == 0 {
		lines = append(lines, "  没有已登记的 agent")
	}
	for _, a := range v.Agents {
		pending := fmt.Sprint(a.Pending)
		if a.Pending < 0 {
			pending = "读取失败"
		}
		lines = append(lines, fmt.Sprintf("  %-16s %-8s  %s / %s", a.Handle, a.Pane, a.Wake, pending))
		if a.Err != "" {
			lines = append(lines, "    错误: "+a.Err)
		}
	}
	b := v.Bridge
	lines = append(lines, "", "跨机 bridge")
	if !b.Configured {
		lines = append(lines, "  未配对")
	} else {
		state := "未运行"
		if b.Running {
			state = "运行中"
		}
		if b.Running && (b.Runner == nil || time.Since(b.Runner.LastTick) > 30*time.Second) {
			state = "持锁，但进度陈旧或未知"
		}
		lines = append(lines, "  "+b.Host+" · "+state, "  relay: "+b.Probe)
		if r := b.Runner; r != nil {
			lines = append(lines, "  最近 tick: "+ageOf(r.LastTick))
			if r.Version != b.Adapter {
				lines = append(lines, "  runner 版本: "+r.Version+"（与当前构建不同，需重新启动后生效）")
			}
			if r.LastError != "" {
				lines = append(lines, "  最近错误 ("+ageOf(r.LastErrorAt)+"): "+r.LastError)
			}
			for _, item := range r.Stuck {
				lines = append(lines, "  需人工处理: "+item)
			}
		}
		lines = append(lines, "", "远端路由 · 最近成功同步（不代表 agent 在线）")
		for _, p := range b.Peers {
			age := ageOf(p.InventoryUpdatedAt)
			if p.InventoryUpdatedAt.IsZero() {
				age = "尚无同步时间"
			}
			lines = append(lines, "  "+p.Host+" · "+age)
			if len(p.Agents) == 0 {
				lines = append(lines, "    没有已知 agent")
			}
			for _, a := range p.Agents {
				lines = append(lines, "    "+bridge.AliasHandle(p.Host, a))
			}
		}
		lines = append(lines, "", "积压 · 消息数")
		for _, q := range []struct {
			name string
			m    map[string]int
		}{{"alias 待转发", b.AliasPending}, {"spool 待推送", b.Pending}, {"隔离", b.Quarantined}} {
			if q.m == nil {
				lines = append(lines, "  "+q.name+": 读取失败")
				continue
			}
			keys := make([]string, 0, len(q.m))
			for k := range q.m {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			if len(keys) == 0 {
				lines = append(lines, "  "+q.name+": 0")
			}
			for _, k := range keys {
				count := fmt.Sprint(q.m[k])
				if q.m[k] < 0 {
					count = "读取失败"
				}
				lines = append(lines, "  "+q.name+" "+k+": "+count)
			}
		}
	}
	for _, err := range append(append([]string{}, v.Errors...), b.Errors...) {
		lines = append(lines, "错误: "+err)
	}
	lines = append(lines, "", "注：已推送、已注入均不等于对端已读取。")
	for i, s := range lines {
		lines[i] = strings.Map(func(r rune) rune {
			if unicode.IsControl(r) {
				return ' '
			}
			return r
		}, s)
	}
	return lines
}

func stty(args ...string) (string, error) {
	cmd := exec.Command("stty", args...)
	cmd.Stdin = os.Stdin
	b, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("stty: %w: %s", err, b)
	}
	return strings.TrimSpace(string(b)), nil
}

func runPopup(args []string) error {
	fs := flag.NewFlagSet("status-popup", flag.ContinueOnError)
	once := fs.Bool("once", false, "print one read-only snapshot without terminal controls")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected status-popup arguments")
	}
	e, err := loadEnv()
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if *once {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		v := inspectPopup(ctx, e)
		fmt.Println(strings.Join(popupLines(v), "\n"))
		if len(v.Errors)+len(v.Bridge.Errors) > 0 {
			return errors.New("status snapshot is incomplete")
		}
		return nil
	}
	saved, err := stty("-g")
	if err != nil {
		return fmt.Errorf("popup needs a terminal; use --once: %w", err)
	}
	if _, err := stty("-icanon", "-echo", "min", "1", "time", "0"); err != nil {
		return err
	}
	defer stty(saved)
	fmt.Print("\x1b[?1049h\x1b[?25l")
	defer fmt.Print("\x1b[?25h\x1b[?1049l")
	keys := make(chan byte, 8)
	go func() {
		defer close(keys)
		var b [1]byte
		for {
			_, err := io.ReadFull(os.Stdin, b[:])
			if err != nil {
				return
			}
			select {
			case keys <- b[0]:
			case <-ctx.Done():
				return
			}
		}
	}()
	updates := make(chan popupView, 1)
	refresh := make(chan struct{}, 1)
	go func() {
		for {
			c, cancel := context.WithTimeout(ctx, 10*time.Second)
			v := inspectPopup(c, e)
			cancel()
			select {
			case updates <- v:
			case <-ctx.Done():
				return
			}
			select {
			case <-time.After(5 * time.Second):
			case <-refresh:
			case <-ctx.Done():
				return
			}
		}
	}()
	lines := []string{"AMQ 通信状态", "正在读取…"}
	offset := 0
	draw := func() {
		rows, cols := 24, 100
		if size, err := stty("size"); err == nil {
			fmt.Sscanf(size, "%d %d", &rows, &cols)
		}
		if rows < 4 {
			rows = 4
		}
		if cols < 10 {
			cols = 10
		}
		page := rows - 2
		visible := wrapLines(lines, cols-1)
		if offset > len(visible)-page {
			offset = max(0, len(visible)-page)
		}
		fmt.Print("\x1b[H\x1b[2J")
		for _, line := range visible[offset:min(len(visible), offset+page)] {
			fmt.Print(line, "\n")
		}
		fmt.Printf("\x1b[%d;1H%s", rows, cropLine("q / Esc 关闭 · r 刷新 · j/k 滚动", cols-1))
	}
	draw()
	for {
		select {
		case <-ctx.Done():
			return nil
		case v := <-updates:
			lines = popupLines(v)
			draw()
		case k, ok := <-keys:
			if !ok || k == 'q' || k == 27 {
				return nil
			}
			switch k {
			case 'r':
				select {
				case refresh <- struct{}{}:
				default:
				}
			case 'j':
				offset++
			case 'k':
				offset = max(0, offset-1)
			}
			draw()
		}
	}
}

func wrapLines(lines []string, columns int) []string {
	var out []string
	for _, line := range lines {
		if line == "" {
			out = append(out, "")
			continue
		}
		for line != "" {
			part := cropLine(line, columns)
			out = append(out, part)
			line = line[len(part):]
		}
	}
	return out
}

// The popup uses plain text; reserve two columns for the CJK labels and emoji.
func cropLine(s string, columns int) string {
	width := 0
	var b strings.Builder
	for _, r := range s {
		n := 1
		if unicode.Is(unicode.Mn, r) {
			n = 0
		} else if r >= 0x1100 && (r <= 0x115f || r >= 0x2e80 && r <= 0xa4cf || r >= 0xac00 && r <= 0xd7a3 || r >= 0xf900 && r <= 0xfaff || r >= 0xfe10 && r <= 0xfe6f || r >= 0xff00 && r <= 0xff60 || r >= 0x1f300) {
			n = 2
		}
		if width+n > columns {
			break
		}
		b.WriteRune(r)
		width += n
	}
	return b.String()
}
