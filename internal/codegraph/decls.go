package codegraph

import (
	"go/ast"
)

// FileOfFunc 返回函数声明所在文件与行号。
func (g *Graph) FileOfFunc(symbolID string) (string, int) {
	for _, p := range g.pkgs {
		for _, f := range p.Syntax {
			for _, d := range f.Decls {
				fd, ok := d.(*ast.FuncDecl)
				if !ok {
					continue
				}
				if g.funcDeclIDQuiet(p, fd) == symbolID {
					pos := g.Fset.Position(fd.Pos())
					return pos.Filename, pos.Line
				}
			}
		}
	}
	return "", 0
}
