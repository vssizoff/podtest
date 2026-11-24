package statement

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
)

const Version = "markdown-katex-0.16.47-v1"

var ErrInvalid = errors.New("invalid document or unsupported math syntax")

type Renderer struct {
	slots  chan struct{}
	script string
}
type Document struct {
	HTML            string `json:"html"`
	Digest          string `json:"digest"`
	RendererVersion string `json:"renderer_version"`
}

func NewRenderer() *Renderer {
	return &Renderer{make(chan struct{}, 2), os.Getenv("MATH_RENDERER_SCRIPT")}
}
func (r *Renderer) Render(ctx context.Context, format, source string) (Document, error) {
	if len(source) > 65536 {
		return Document{}, ErrInvalid
	}
	var output string
	switch format {
	case "latex":
		var err error
		output, err = r.math(ctx, source, true)
		if err != nil {
			return Document{}, err
		}
	case "markdown":
		markdown := goldmark.New()
		src := []byte(source)
		root := markdown.Parser().Parse(text.NewReader(src))
		// Default Goldmark drops raw HTML. Omit URL-bearing nodes as well until asset ACLs exist.
		var images []ast.Node
		_ = ast.Walk(root, func(n ast.Node, enter bool) (ast.WalkStatus, error) {
			if enter {
				switch n.(type) {
				case *ast.Image:
					images = append(images, n)
				case *ast.Link:
					n.(*ast.Link).Destination = nil
				case *ast.AutoLink:
					images = append(images, n)
				}
			}
			return ast.WalkContinue, nil
		})
		for _, n := range images {
			n.Parent().RemoveChild(n.Parent(), n)
		}
		var html bytes.Buffer
		if err := markdown.Renderer().Render(&html, src, root); err != nil {
			return Document{}, err
		}
		output = html.String()
		if r.script != "" && strings.Contains(output, "$") {
			var err error
			output, err = r.math(ctx, output, false)
			if err != nil {
				return Document{}, err
			}
		}
	default:
		return Document{}, ErrInvalid
	}
	sum := sha256.Sum256([]byte(Version + "\x00" + format + "\x00" + source))
	return Document{output, hex.EncodeToString(sum[:]), Version}, nil
}
func (r *Renderer) math(ctx context.Context, source string, standalone bool) (string, error) {
	if r.script == "" {
		return "", errors.New("math renderer unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	select {
	case r.slots <- struct{}{}:
		defer func() { <-r.slots }()
	case <-ctx.Done():
		return "", ctx.Err()
	}
	mode := "markdown"
	if standalone {
		mode = "latex"
	}
	cmd := exec.CommandContext(ctx, "node", "--max-old-space-size=96", r.script, mode)
	cmd.Env = []string{"NODE_ENV=production"}
	cmd.Stdin = strings.NewReader(source)
	out, err := cmd.Output()
	if err != nil || len(out) > 2<<20 {
		return "", ErrInvalid
	}
	return string(out), nil
}
