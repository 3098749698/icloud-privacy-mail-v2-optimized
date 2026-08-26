package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"icloud-privacy-mail-v2/internal/config"
	"icloud-privacy-mail-v2/internal/domain"
	"icloud-privacy-mail-v2/internal/mailbox"
	"icloud-privacy-mail-v2/internal/scheduler"
	"icloud-privacy-mail-v2/internal/store"
)

const defaultDeleteDelay = 2 * time.Second

type maintenanceOptions struct {
	ConfigPath        string
	DataPath          string
	AccountID         string
	Limit             int
	Delay             time.Duration
	Execute           bool
	IncludeLegacyUsed bool
}

type maintenanceSummary struct {
	Candidates int
	Attempted  int
	Deleted    int
	Failed     int
}

type deleteRemoteFunc func(context.Context, string) error

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "维护任务失败：", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	options, err := parseOptions(args, stderr)
	if err != nil {
		return err
	}
	cfg, err := loadConfig(options)
	if err != nil {
		return fmt.Errorf("加载配置失败：%w", err)
	}
	state, err := store.Open(cfg.DataPath)
	if err != nil {
		return fmt.Errorf("打开状态文件失败：%w", err)
	}

	candidates := exportedMailboxCandidates(state.Snapshot(), options.AccountID, options.Limit, options.IncludeLegacyUsed)
	service := mailbox.NewService(cfg, state)
	schedulerService := scheduler.NewService(state, service)
	service.SetCapacityReleasedHandler(func(accountID string) {
		schedulerService.NotifyCapacityReleased(accountID)
	})
	_, err = processCandidates(ctx, candidates, options, service.DeleteRemote, stdout)
	return err
}

func parseOptions(args []string, stderr io.Writer) (maintenanceOptions, error) {
	options := maintenanceOptions{ConfigPath: "config.json", Delay: defaultDeleteDelay}
	flags := flag.NewFlagSet("ipm-maintenance", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&options.ConfigPath, "config", options.ConfigPath, "配置文件路径")
	flags.StringVar(&options.DataPath, "data", "", "覆盖状态文件路径")
	flags.StringVar(&options.AccountID, "account-id", "", "仅处理指定 Apple 账号")
	flags.IntVar(&options.Limit, "limit", 0, "最多处理数量，0 表示全部")
	flags.DurationVar(&options.Delay, "delay", options.Delay, "每次远端删除之间的间隔")
	flags.BoolVar(&options.Execute, "execute", false, "实际执行远端删除；默认仅预览")
	flags.BoolVar(&options.IncludeLegacyUsed, "include-legacy-used", false, "同时处理没有导出时间的旧版已使用记录")
	if err := flags.Parse(args); err != nil {
		return maintenanceOptions{}, err
	}
	if flags.NArg() != 0 {
		return maintenanceOptions{}, fmt.Errorf("存在未识别参数：%s", strings.Join(flags.Args(), " "))
	}
	if options.Limit < 0 {
		return maintenanceOptions{}, errors.New("--limit 不能小于 0")
	}
	if options.Delay < 0 {
		return maintenanceOptions{}, errors.New("--delay 不能小于 0")
	}
	options.ConfigPath = strings.TrimSpace(options.ConfigPath)
	options.DataPath = strings.TrimSpace(options.DataPath)
	options.AccountID = strings.TrimSpace(options.AccountID)
	return options, nil
}

func loadConfig(options maintenanceOptions) (config.Config, error) {
	cfg, err := config.Load(options.ConfigPath)
	if err != nil {
		return config.Config{}, err
	}
	if options.DataPath != "" {
		cfg.DataPath = options.DataPath
	}
	return cfg, nil
}

// exportedMailboxCandidates selects only records that can be proven to have
// been exported and are not currently owned by any mailbox lease.
func exportedMailboxCandidates(state domain.State, accountID string, limit int, includeLegacyUsed bool) []domain.Mailbox {
	accountID = strings.TrimSpace(accountID)
	claimed := make(map[string]struct{}, len(state.MailboxLeases))
	for _, lease := range state.MailboxLeases {
		if lease.State == domain.MailboxLeaseClaimed {
			claimed[strings.TrimSpace(lease.MailboxID)] = struct{}{}
		}
	}

	candidates := make([]domain.Mailbox, 0)
	for _, candidate := range state.Mailboxes {
		if candidate.Status != domain.StatusUsed || (candidate.ExportedAt.IsZero() && !includeLegacyUsed) {
			continue
		}
		if accountID != "" && candidate.AccountID != accountID {
			continue
		}
		if strings.TrimSpace(candidate.ActiveLeaseID) != "" {
			continue
		}
		if _, leased := claimed[candidate.ID]; leased {
			continue
		}
		candidates = append(candidates, candidate)
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].ExportedAt.Equal(candidates[j].ExportedAt) {
			return candidates[i].ID < candidates[j].ID
		}
		return candidates[i].ExportedAt.Before(candidates[j].ExportedAt)
	})
	if limit > 0 && len(candidates) > limit {
		candidates = candidates[:limit]
	}
	return candidates
}

func processCandidates(ctx context.Context, candidates []domain.Mailbox, options maintenanceOptions, deleteRemote deleteRemoteFunc, output io.Writer) (maintenanceSummary, error) {
	summary := maintenanceSummary{Candidates: len(candidates)}
	mode := "预览"
	if options.Execute {
		mode = "执行"
	}
	fmt.Fprintf(output, "模式：%s；候选：%d；账号过滤：%s；间隔：%s\n", mode, len(candidates), displayAccountFilter(options.AccountID), options.Delay)
	if !options.Execute {
		for index, candidate := range candidates {
			exportedAt := "legacy-used"
			if !candidate.ExportedAt.IsZero() {
				exportedAt = candidate.ExportedAt.Format(time.RFC3339)
			}
			fmt.Fprintf(output, "[预览 %d/%d] %s | %s | account=%s | exported_at=%s\n", index+1, len(candidates), candidate.ID, candidate.Email, candidate.AccountID, exportedAt)
		}
		fmt.Fprintln(output, "汇总：未执行远端删除；需要执行时显式添加 --execute，并确保主服务已停止。")
		return summary, nil
	}
	if deleteRemote == nil {
		return summary, errors.New("远端删除函数未初始化")
	}

	for index, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		if index > 0 && options.Delay > 0 {
			timer := time.NewTimer(options.Delay)
			select {
			case <-ctx.Done():
				if !timer.Stop() {
					<-timer.C
				}
				return summary, ctx.Err()
			case <-timer.C:
			}
		}
		summary.Attempted++
		fmt.Fprintf(output, "[执行 %d/%d] %s | %s | account=%s ... ", index+1, len(candidates), candidate.ID, candidate.Email, candidate.AccountID)
		if err := deleteRemote(ctx, candidate.ID); err != nil {
			summary.Failed++
			fmt.Fprintf(output, "失败（%s）\n", safeErrorText(err))
			continue
		}
		summary.Deleted++
		fmt.Fprintln(output, "成功")
	}
	fmt.Fprintf(output, "汇总：候选=%d，尝试=%d，成功=%d，失败=%d\n", summary.Candidates, summary.Attempted, summary.Deleted, summary.Failed)
	if summary.Failed > 0 {
		return summary, fmt.Errorf("%d 个邮箱远端删除失败", summary.Failed)
	}
	return summary, nil
}

func displayAccountFilter(accountID string) string {
	if strings.TrimSpace(accountID) == "" {
		return "全部"
	}
	return strings.TrimSpace(accountID)
}

func safeErrorText(err error) string {
	if err == nil {
		return ""
	}
	message := strings.TrimSpace(err.Error())
	lower := strings.ToLower(message)
	for _, sensitive := range []string{"token", "cookie", "authorization", "api_key", "api key"} {
		if strings.Contains(lower, sensitive) {
			return "远端响应包含敏感字段，详情已隐藏"
		}
	}
	const maxLength = 300
	if len(message) > maxLength {
		message = message[:maxLength] + "..."
	}
	return message
}
