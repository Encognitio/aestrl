package tui

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/Encognitio/aestrl/pkg/engine"
	"github.com/Encognitio/aestrl/pkg/p2p"
)

// Invoker abstracts local or remote execution of core operations.
type Invoker interface {
	Execute(op string, args map[string]interface{}) (interface{}, error)
}

// EngineInvoker wraps a local engine.
type EngineInvoker struct {
	Eng *engine.Engine
}

func (ei *EngineInvoker) Execute(op string, args map[string]interface{}) (interface{}, error) {
	return ei.Eng.Execute(op, args)
}

// P2PInvoker wraps a remote p2p.ClientSession.
type P2PInvoker struct {
	Session *p2p.ClientSession
}

func (pi *P2PInvoker) Execute(op string, args map[string]interface{}) (interface{}, error) {
	return pi.Session.Execute(context.Background(), op, args)
}

// Run launches the interactive terminal interface.
func Run(invoker Invoker, title string, in io.Reader, out io.Writer) error {
	reader := bufio.NewReader(in)
	currentPath := ""

	for {
		// Fetch current directory contents
		res, err := invoker.Execute("ls", map[string]interface{}{"path": currentPath})
		if err != nil {
			fmt.Fprintf(out, "Error listing %q: %v\n", currentPath, err)
			return err
		}

		items := parseItems(res)

		// Print Header
		fmt.Fprintln(out, "\n==================================================================")
		fmt.Fprintf(out, " AESTRL INSPECT — %s\n", title)
		displayPath := "/"
		if currentPath != "" {
			displayPath = "/" + currentPath
		}
		fmt.Fprintf(out, " Location: %s (%d items)\n", displayPath, len(items))
		fmt.Fprintln(out, "==================================================================")

		// Print Items
		if currentPath != "" {
			fmt.Fprintln(out, "  [ 0] 📁 .. (parent directory)")
		}

		for i, it := range items {
			idx := i + 1
			if it.IsDir {
				fmt.Fprintf(out, "  [%2d] 📁 %s/\n", idx, it.Name)
			} else {
				fmt.Fprintf(out, "  [%2d] 📄 %s (%d bytes)\n", idx, it.Name, it.Size)
			}
		}

		fmt.Fprintln(out, "------------------------------------------------------------------")
		fmt.Fprintln(out, " Commands: [num] Open | [..] Up | [/] Search | [c] Context")
		fmt.Fprintln(out, "           [t] Tree   | [o] Outline | [m] Manifest | [s] Symbol | [q] Quit")
		fmt.Fprint(out, " > ")

		line, err := reader.ReadString('\n')
		if err != nil {
			break
		}
		cmd := strings.TrimSpace(line)
		if cmd == "" {
			continue
		}

		switch {
		case cmd == "q" || cmd == "quit" || cmd == "exit":
			fmt.Fprintln(out, "Exiting Aestrl inspection.")
			return nil

		case cmd == ".." || cmd == "0":
			if currentPath != "" {
				parts := strings.Split(strings.Trim(currentPath, "/"), "/")
				if len(parts) <= 1 {
					currentPath = ""
				} else {
					currentPath = strings.Join(parts[:len(parts)-1], "/")
				}
			}

		case cmd == "t" || cmd == "tree":
			treeRes, err := invoker.Execute("tree", map[string]interface{}{"path": currentPath, "depth": 3})
			if err != nil {
				fmt.Fprintf(out, "Tree error: %v\n", err)
			} else {
				printTree(out, treeRes, "")
			}
			promptContinue(reader, out)

		case cmd == "m" || cmd == "manifest":
			manRes, err := invoker.Execute("manifest", nil)
			if err != nil {
				fmt.Fprintf(out, "Manifest error: %v\n", err)
			} else {
				data, _ := json.MarshalIndent(manRes, "", "  ")
				fmt.Fprintln(out, string(data))
			}
			promptContinue(reader, out)

		case strings.HasPrefix(cmd, "/") || strings.HasPrefix(cmd, "search "):
			query := strings.TrimPrefix(cmd, "/")
			query = strings.TrimPrefix(query, "search ")
			query = strings.TrimSpace(query)
			if query == "" {
				fmt.Fprint(out, "Enter search query: ")
				qLine, _ := reader.ReadString('\n')
				query = strings.TrimSpace(qLine)
			}
			if query != "" {
				sRes, err := invoker.Execute("search", map[string]interface{}{"query": query, "path": currentPath})
				if err != nil {
					fmt.Fprintf(out, "Search error: %v\n", err)
				} else {
					printSearchResults(out, sRes)
				}
				promptContinue(reader, out)
			}

		case strings.HasPrefix(cmd, "c ") || cmd == "c" || cmd == "context":
			query := strings.TrimPrefix(cmd, "c")
			query = strings.TrimPrefix(query, "ontext")
			query = strings.TrimSpace(query)
			if query == "" {
				fmt.Fprint(out, "Enter context query: ")
				qLine, _ := reader.ReadString('\n')
				query = strings.TrimSpace(qLine)
			}
			if query != "" {
				ctxRes, err := invoker.Execute("context", map[string]interface{}{"query": query, "path": currentPath})
				if err != nil {
					fmt.Fprintf(out, "Context error: %v\n", err)
				} else {
					printContextResults(out, ctxRes)
				}
				promptContinue(reader, out)
			}

		case strings.HasPrefix(cmd, "o ") || cmd == "o" || cmd == "outline":
			path := strings.TrimPrefix(cmd, "o")
			path = strings.TrimPrefix(path, "utline")
			path = strings.TrimSpace(path)
			if path == "" {
				fmt.Fprint(out, "Enter file path for outline: ")
				pLine, _ := reader.ReadString('\n')
				path = strings.TrimSpace(pLine)
			}
			if path != "" {
				outRes, err := invoker.Execute("outline", map[string]interface{}{"path": path})
				if err != nil {
					fmt.Fprintf(out, "Outline error: %v\n", err)
				} else {
					data, _ := json.MarshalIndent(outRes, "", "  ")
					fmt.Fprintln(out, string(data))
				}
				promptContinue(reader, out)
			}

		case strings.HasPrefix(cmd, "s ") || cmd == "s" || cmd == "symbol":
			sym := strings.TrimPrefix(cmd, "s")
			sym = strings.TrimPrefix(sym, "ymbol")
			sym = strings.TrimSpace(sym)
			if sym == "" {
				fmt.Fprint(out, "Enter symbol name: ")
				sLine, _ := reader.ReadString('\n')
				sym = strings.TrimSpace(sLine)
			}
			if sym != "" {
				symRes, err := invoker.Execute("symbol", map[string]interface{}{"name": sym})
				if err != nil {
					fmt.Fprintf(out, "Symbol error: %v\n", err)
				} else {
					data, _ := json.MarshalIndent(symRes, "", "  ")
					fmt.Fprintln(out, string(data))
				}
				promptContinue(reader, out)
			}

		case strings.HasPrefix(cmd, "g ") || strings.HasPrefix(cmd, "grep ") || strings.HasPrefix(cmd, "rg "):
			pattern := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(strings.TrimPrefix(cmd, "grep"), "rg"), "g"))
			if pattern != "" {
				gRes, err := invoker.Execute("grep", map[string]interface{}{"query": pattern, "path": currentPath})
				if err != nil {
					fmt.Fprintf(out, "Grep error: %v\n", err)
				} else {
					data, _ := json.MarshalIndent(gRes, "", "  ")
					fmt.Fprintln(out, string(data))
				}
				promptContinue(reader, out)
			}

		case strings.HasPrefix(cmd, "f ") || strings.HasPrefix(cmd, "find "):
			pat := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(cmd, "find"), "f"))
			if pat != "" {
				fRes, err := invoker.Execute("find", map[string]interface{}{"query": pat, "path": currentPath})
				if err != nil {
					fmt.Fprintf(out, "Find error: %v\n", err)
				} else {
					data, _ := json.MarshalIndent(fRes, "", "  ")
					fmt.Fprintln(out, string(data))
				}
				promptContinue(reader, out)
			}

		case strings.HasPrefix(cmd, "r ") || strings.HasPrefix(cmd, "refs "):
			name := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(cmd, "refs"), "r"))
			if name != "" {
				rRes, err := invoker.Execute("refs", map[string]interface{}{"name": name})
				if err != nil {
					fmt.Fprintf(out, "Refs error: %v\n", err)
				} else {
					data, _ := json.MarshalIndent(rRes, "", "  ")
					fmt.Fprintln(out, string(data))
				}
				promptContinue(reader, out)
			}

		case strings.HasPrefix(cmd, "deps ") || cmd == "deps":
			path := strings.TrimSpace(strings.TrimPrefix(cmd, "deps"))
			if path == "" {
				path = currentPath
			}
			dRes, err := invoker.Execute("deps", map[string]interface{}{"path": path})
			if err != nil {
				fmt.Fprintf(out, "Deps error: %v\n", err)
			} else {
				data, _ := json.MarshalIndent(dRes, "", "  ")
				fmt.Fprintln(out, string(data))
			}
			promptContinue(reader, out)

		case strings.HasPrefix(cmd, "stat "):
			path := strings.TrimSpace(strings.TrimPrefix(cmd, "stat"))
			stRes, err := invoker.Execute("stat", map[string]interface{}{"path": path})
			if err != nil {
				fmt.Fprintf(out, "Stat error: %v\n", err)
			} else {
				data, _ := json.MarshalIndent(stRes, "", "  ")
				fmt.Fprintln(out, string(data))
			}
			promptContinue(reader, out)

		case strings.HasPrefix(cmd, "compare ") || strings.HasPrefix(cmd, "diff "):
			parts := strings.Fields(cmd)
			if len(parts) >= 3 {
				cmpRes, err := invoker.Execute("compare", map[string]interface{}{"path1": parts[1], "path2": parts[2]})
				if err != nil {
					fmt.Fprintf(out, "Compare error: %v\n", err)
				} else {
					data, _ := json.MarshalIndent(cmpRes, "", "  ")
					fmt.Fprintln(out, string(data))
				}
				promptContinue(reader, out)
			}

		case strings.HasPrefix(cmd, "raw ") || strings.HasPrefix(cmd, "get "):
			path := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(cmd, "raw"), "get"))
			rawRes, err := invoker.Execute("raw", map[string]interface{}{"path": path})
			if err != nil {
				fmt.Fprintf(out, "Raw error: %v\n", err)
			} else {
				fmt.Fprintf(out, "Retrieved raw file: %+v\n", rawRes)
			}
			promptContinue(reader, out)

		default:
			// Parse item number selection
			idx, err := strconv.Atoi(cmd)
			if err != nil || idx < 1 || idx > len(items) {
				fmt.Fprintf(out, "Unknown selection: %s\n", cmd)
				continue
			}

			targetItem := items[idx-1]
			if targetItem.IsDir {
				currentPath = targetItem.Path
			} else {
				// Read and display file
				viewFile(invoker, targetItem.Path, out, reader)
			}
		}
	}

	return nil
}

func viewFile(invoker Invoker, path string, out io.Writer, reader *bufio.Reader) {
	res, err := invoker.Execute("read", map[string]interface{}{"path": path})
	if err != nil {
		fmt.Fprintf(out, "Error reading %s: %v\n", path, err)
		promptContinue(reader, out)
		return
	}

	fmt.Fprintln(out, "\n------------------------------------------------------------------")
	fmt.Fprintf(out, " File: %s\n", path)
	fmt.Fprintln(out, "------------------------------------------------------------------")

	if m, ok := res.(map[string]interface{}); ok {
		if content, ok := m["content"].(string); ok {
			lines := strings.Split(content, "\n")
			for i, line := range lines {
				fmt.Fprintf(out, "%4d | %s\n", i+1, line)
			}
		}
	} else if data, err := json.MarshalIndent(res, "", "  "); err == nil {
		fmt.Fprintln(out, string(data))
	}

	promptContinue(reader, out)
}

func promptContinue(reader *bufio.Reader, out io.Writer) {
	fmt.Fprint(out, "\n[Press Enter to continue]")
	_, _ = reader.ReadString('\n')
}

func parseItems(res interface{}) []engine.FileItem {
	data, err := json.Marshal(res)
	if err != nil {
		return nil
	}
	var items []engine.FileItem
	_ = json.Unmarshal(data, &items)
	return items
}

func printTree(out io.Writer, res interface{}, prefix string) {
	data, _ := json.Marshal(res)
	var node engine.TreeNode
	if err := json.Unmarshal(data, &node); err != nil {
		fmt.Fprintln(out, string(data))
		return
	}
	renderTreeNode(out, &node, prefix)
}

func renderTreeNode(out io.Writer, node *engine.TreeNode, prefix string) {
	if node == nil {
		return
	}
	if node.IsDir {
		fmt.Fprintf(out, "%s📁 %s/\n", prefix, node.Name)
		for _, ch := range node.Children {
			renderTreeNode(out, ch, prefix+"  ")
		}
	} else {
		fmt.Fprintf(out, "%s📄 %s (%d bytes)\n", prefix, node.Name, node.Size)
	}
}

func printSearchResults(out io.Writer, res interface{}) {
	data, _ := json.Marshal(res)
	var results []engine.SearchResult
	if err := json.Unmarshal(data, &results); err != nil {
		fmt.Fprintln(out, string(data))
		return
	}
	fmt.Fprintf(out, "\nFound %d ranked matches:\n", len(results))
	for i, r := range results {
		fmt.Fprintf(out, "\n[%d] %s (Line %d, Score: %.2f) [%s]\n", i+1, r.Path, r.LineNumber, r.Score, r.Kind)
		fmt.Fprintf(out, "    %s\n", r.Excerpt)
	}
}

func printContextResults(out io.Writer, res interface{}) {
	data, _ := json.Marshal(res)
	var cr engine.ContextResult
	if err := json.Unmarshal(data, &cr); err != nil {
		fmt.Fprintln(out, string(data))
		return
	}
	fmt.Fprintf(out, "\nContext Pack: %q (Budget: %d chars, Used: %d chars, Excerpts: %d)\n", cr.Query, cr.Budget, cr.UsedChars, len(cr.Excerpts))
	if cr.Summary != "" {
		fmt.Fprintf(out, "Summary: %s\n", cr.Summary)
	}
	for i, ex := range cr.Excerpts {
		fmt.Fprintf(out, "\n[%d] %s (Line %d, Score: %.2f) [%s]\n    %s\n", i+1, ex.Path, ex.LineNumber, ex.Score, ex.Kind, ex.Excerpt)
	}
}
