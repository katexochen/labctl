package helpers

import "text/template"

type Context struct {
	Command    string // the Kong Command()
	DebugCount int
	Settings   Settings

	TopoFile   string   // used by config, serve
	NodeFilter []string // Used by config

	Topo Topo

	Template *template.Template
}

func (c *Context) Load(filename string) error {
	c.TopoFile = filename
	return c.Topo.Load(c.TopoFile)
}