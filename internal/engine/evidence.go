package engine

import (
	"github.com/specforge/specforge/internal/facts"
	"github.com/specforge/specforge/internal/frontend"
)

// verifyEvidence 编译前的证据闸门（设计思想 4「无证据，不成事实」）：
// 每条证据须指向本次分析文件集内的真实文件、内容指纹一致、行号在文件范围内；
// 一条有效证据都没有的事实被丢弃（编译器随之把依赖它的位置降级为 unknown）。
// 保留下来的证据路径改写为仓库相对路径，使产物与检出目录无关。
// 返回保留的事实与被丢弃事实的 ID（有序，与输入顺序一致）。
func verifyEvidence(prog frontend.Program, in []*facts.Fact) ([]*facts.Fact, []string) {
	out := make([]*facts.Fact, 0, len(in))
	var dropped []string
	for _, f := range in {
		valid := f.Evidence[:0:0]
		for _, ev := range f.Evidence {
			if evidenceValid(prog, ev) {
				ev.File = prog.RelPath(ev.File)
				valid = append(valid, ev)
			}
		}
		if len(valid) == 0 {
			dropped = append(dropped, f.ID)
			continue
		}
		f.Evidence = valid
		out = append(out, f)
	}
	return out, dropped
}

// evidenceValid 单条证据是否可在当前代码上复核：文件可读、指纹匹配（有指纹时）、行号有效。
func evidenceValid(prog frontend.Program, ev facts.Evidence) bool {
	if ev.File == "" {
		return false
	}
	if ev.BlobSHA != "" && ev.BlobSHA != prog.FileHash(ev.File) {
		return false
	}
	return prog.ValidLine(ev.File, ev.StartLine)
}
