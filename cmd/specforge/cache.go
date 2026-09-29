package main

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

// newCacheCmd cache：查看/清理 run 缓存与 LLM 调用缓存。
func newCacheCmd(a *app) *cobra.Command {
	var repo, dir string
	root := func() string {
		if dir != "" {
			return dir
		}
		return defaultCacheRoot(repo)
	}
	cmd := &cobra.Command{
		Use:   "cache",
		Short: "Inspect or clear the run cache and the LLM call cache",
		Long: `SpecForge keeps two caches under <repo>/.specforge/cache (override with --cache-dir):

  memo/  whole-run cache: one entry per input fingerprint (sources, profile, engine, LLM settings)
         holding all outputs; the 16 most recently used entries are kept.
  llm/   one file per LLM request; reused as long as the prompt and the code the model read are unchanged.

Both are safe to delete at any time; the next 'gen' simply recomputes (clearing llm/ costs tokens).
The directory contains no credentials and can be shared across checkouts or cached in CI.`,
		Example: `  specforge cache stats
  specforge cache clean --memo
  specforge cache clean --llm --dry-run`,
		Args: cobra.NoArgs,
	}
	cmd.PersistentFlags().StringVar(&repo, "repo", ".", "repository root (locates the default cache directory)")
	cmd.PersistentFlags().StringVar(&dir, "cache-dir", "", "cache root (default: <repo>/.specforge/cache)")

	stats := &cobra.Command{
		Use:   "stats",
		Short: "Show entry counts and disk usage of both caches",
		Example: `  specforge cache stats
  specforge cache stats --cache-dir /ci/cache/specforge --json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			res := map[string]any{
				"cache_dir": root(),
				memoSubdir:  statDir(filepath.Join(root(), memoSubdir)),
				llmSubdir:   statDir(filepath.Join(root(), llmSubdir)),
			}
			a.emit(res, func(w io.Writer) {
				m, l := res[memoSubdir].(dirStats), res[llmSubdir].(dirStats)
				fmt.Fprintf(w, "cache %s\n", root())
				fmt.Fprintf(w, "  memo  %d entries  %s\n", m.Entries, humanBytes(m.Bytes))
				fmt.Fprintf(w, "  llm   %d entries  %s\n", l.Entries, humanBytes(l.Bytes))
			})
			return nil
		},
	}
	var onlyLLM, onlyMemo, dryRun bool
	clean := &cobra.Command{
		Use:   "clean",
		Short: "Delete cache entries (both caches unless --llm or --memo is given)",
		Long: `Delete cache entries. Without --llm/--memo the whole cache directory is removed.
Use --dry-run to see what would be removed and how much space it frees.`,
		Example: `  specforge cache clean
  specforge cache clean --memo          # force full re-analysis, keep paid LLM answers
  specforge cache clean --llm --dry-run`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			targets := []string{root()}
			switch {
			case onlyLLM:
				targets = []string{filepath.Join(root(), llmSubdir)}
			case onlyMemo:
				targets = []string{filepath.Join(root(), memoSubdir)}
			}
			var freed int64
			for _, t := range targets {
				freed += statDir(t).Bytes
				if !dryRun {
					if err := os.RemoveAll(t); err != nil {
						return newErr(exitInternal, codeInternal, err.Error(), "check directory permissions")
					}
				}
			}
			res := map[string]any{"removed": targets, "bytes": freed, "dry_run": dryRun}
			a.emit(res, func(w io.Writer) {
				verb := "removed"
				if dryRun {
					verb = "would remove"
				}
				fmt.Fprintf(w, "%s %s (%s)\n", verb, strings.Join(targets, ", "), humanBytes(freed))
			})
			return nil
		},
	}
	clean.Flags().BoolVar(&onlyLLM, "llm", false, "only clear the LLM call cache (next --llm run pays for all calls again)")
	clean.Flags().BoolVar(&onlyMemo, "memo", false, "only clear the run cache (next gen re-analyzes; LLM answers stay cached)")
	clean.Flags().BoolVar(&dryRun, "dry-run", false, "show what would be removed without deleting")
	clean.MarkFlagsMutuallyExclusive("llm", "memo")
	cmd.AddCommand(stats, clean)
	return cmd
}

// dirStats 目录统计。
type dirStats struct {
	Entries int   `json:"entries"` // 条目数（memo：指纹目录数；llm：缓存文件数）
	Bytes   int64 `json:"bytes"`   // 占用字节
}

// statDir 统计目录下的条目数与总大小（memo 以一级子目录计条目）。
func statDir(dir string) dirStats {
	var st dirStats
	isMemo := filepath.Base(dir) == memoSubdir
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if isMemo && filepath.Dir(p) == dir {
				st.Entries++
			}
			return nil
		}
		if info, err := d.Info(); err == nil {
			st.Bytes += info.Size()
		}
		if !isMemo {
			st.Entries++
		}
		return nil
	})
	return st
}

// byteUnit 字节换算基数。
const byteUnit = 1024

// humanBytes 字节数的人类可读形式。
func humanBytes(n int64) string {
	if n < byteUnit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(byteUnit), 0
	for m := n / byteUnit; m >= byteUnit; m /= byteUnit {
		div *= byteUnit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
