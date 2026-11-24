package cmd

import (
	"regexp"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// A small Markdown renderer for text pages (a pull request's body): headings,
// bullets, quotes, rules, fenced code and inline `code`, **bold**, *italic*
// and [links](url), as terminal styles. Not full CommonMark: what a PR
// description mostly uses.

var (
	mdHeading  = lipgloss.NewStyle().Foreground(lipgloss.Color("51")).Bold(true)
	mdCode     = lipgloss.NewStyle().Foreground(lipgloss.Color("222"))
	mdBold     = lipgloss.NewStyle().Bold(true)
	mdItalic   = lipgloss.NewStyle().Italic(true)
	mdLink     = lipgloss.NewStyle().Foreground(lipgloss.Color("33")).Underline(true)
	mdQuote    = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	mdComment  = regexp.MustCompile(`(?s)<!--.*?-->`)
	mdHeadRe   = regexp.MustCompile(`^\s{0,3}(#{1,6})\s+(.*?)\s*#*\s*$`)
	mdBulletRe = regexp.MustCompile(`^(\s*)[-*+]\s+(\[[ xX]\]\s+)?(.*)$`)
	mdRuleRe   = regexp.MustCompile(`^\s{0,3}([-*_])(\s*[-*_]){2,}\s*$`)
	mdInlineRe = regexp.MustCompile("`([^`]+)`|\\*\\*([^*]+)\\*\\*|\\[([^\\]]+)\\]\\(([^)\\s]+)\\)|(?:^|\\s)\\*([^*\\s][^*]*)\\*")
)

// markdownLines turns Markdown source into lines of a text page with styled
// spans; each is one source line (the page wraps them later). width is only
// used for rules.
func markdownLines(src []string, width int) []string {
	var out []string
	fenced := false
	for _, l := range strings.Split(mdComment.ReplaceAllString(strings.Join(src, "\n"), ""), "\n") {
		l = strings.TrimRight(strings.ReplaceAll(l, "\t", "    "), " \r")
		if t := strings.TrimSpace(l); strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			fenced = !fenced
			continue
		}
		switch {
		case fenced:
			out = append(out, "    "+mdCode.Render(l))
		case mdHeadRe.MatchString(l):
			out = append(out, mdHeading.Render(mdHeadRe.FindStringSubmatch(l)[2]))
		case mdRuleRe.MatchString(l):
			out = append(out, mdQuote.Render(strings.Repeat("─", max(min(width, 60), 3))))
		case mdBulletRe.MatchString(l):
			m := mdBulletRe.FindStringSubmatch(l)
			mark := "•"
			if m[2] != "" {
				mark = "☐"
				if strings.ContainsAny(m[2], "xX") {
					mark = "☑"
				}
			}
			out = append(out, m[1]+mark+" "+mdInline(m[3]))
		case strings.HasPrefix(strings.TrimSpace(l), ">"):
			out = append(out, mdQuote.Render("▎ "+strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(l), ">"))))
		default:
			out = append(out, mdInline(l))
		}
	}
	// a comment removed from between blank lines can leave a run of them
	var res []string
	for _, l := range out {
		if l == "" && len(res) > 0 && res[len(res)-1] == "" {
			continue
		}
		res = append(res, l)
	}
	return res
}

func mdInline(s string) string {
	return mdInlineRe.ReplaceAllStringFunc(s, func(x string) string {
		m := mdInlineRe.FindStringSubmatch(x)
		switch {
		case m[1] != "":
			return mdCode.Render(m[1])
		case m[2] != "":
			return mdBold.Render(m[2])
		case m[3] != "":
			return mdLink.Render(m[3]) + " (" + m[4] + ")"
		default:
			return x[:len(x)-len(m[5])-2] + mdItalic.Render(m[5])
		}
	})
}
