package main

import (
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// flagGroupAnnotation 选项分组的 pflag 注解键：帮助里按组分节展示，避免十几个选项混在一起。
const flagGroupAnnotation = "specforge_group"

// defaultFlagGroup 未分组选项所在的节标题。
const defaultFlagGroup = "Flags"

// setGroup 把 names 指定的选项归入 group 节。
func setGroup(fs *pflag.FlagSet, group string, names ...string) {
	for _, n := range names {
		_ = fs.SetAnnotation(n, flagGroupAnnotation, []string{group})
	}
}

// usageTemplate cobra 默认模板的精简版：本地选项按组分节（groupedFlags）。
const usageTemplate = `Usage:{{if .Runnable}}
  {{.UseLine}}{{end}}{{if .HasAvailableSubCommands}}
  {{.CommandPath}} [command]{{end}}{{if .HasExample}}

Examples:
{{.Example}}{{end}}{{if .HasAvailableSubCommands}}

Available Commands:{{range .Commands}}{{if (or .IsAvailableCommand (eq .Name "help"))}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}{{end}}{{if .HasAvailableLocalFlags}}

{{groupedFlags .}}{{end}}{{if .HasAvailableInheritedFlags}}

Global Flags:
{{.InheritedFlags.FlagUsages | trimTrailingWhitespaces}}{{end}}{{if .HasAvailableSubCommands}}

Use "{{.CommandPath}} [command] --help" for more information about a command.{{end}}
`

// installHelp 为整棵命令树安装分组帮助模板。
func installHelp(root *cobra.Command) {
	cobra.AddTemplateFunc("groupedFlags", groupedFlags)
	root.SetUsageTemplate(usageTemplate)
}

// groupedFlags 按注解分组渲染本地选项（组顺序 = 首个成员的注册顺序，未分组者归入 "Flags" 且放最后）。
func groupedFlags(c *cobra.Command) string {
	var order []string
	local := c.LocalFlags()
	local.SortFlags = c.Flags().SortFlags
	sets := map[string]*pflag.FlagSet{}
	local.VisitAll(func(f *pflag.Flag) {
		if f.Hidden {
			return
		}
		g := defaultFlagGroup
		if v := f.Annotations[flagGroupAnnotation]; len(v) > 0 {
			g = v[0]
		}
		if sets[g] == nil {
			sets[g] = pflag.NewFlagSet(g, pflag.ContinueOnError)
			order = append(order, g)
		}
		sets[g].AddFlag(f)
	})
	if sets[defaultFlagGroup] != nil && len(order) > 1 { // 未分组（通常只有 --help）放最后
		for i, g := range order {
			if g == defaultFlagGroup {
				order = append(append(order[:i:i], order[i+1:]...), g)
				break
			}
		}
	}
	var b strings.Builder
	for i, g := range order {
		if i > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(g + ":\n")
		b.WriteString(strings.TrimRight(sets[g].FlagUsages(), " \n"))
	}
	return b.String()
}
