package main

import (
	"regexp"
	"regexp/syntax"
	"strings"
)

func CompileExtGlob(extglob string) (*regexp.Regexp, error) {
	ctx := globctx{glob: extglob}
	ctx.compileGlobstarPrefix()

	if err := ctx.compileExpression(); err != nil {
		return nil, err
	}

	return regexp.Compile("^" + string(ctx.regexp) + "$")
}

type globctx struct {
	glob       string
	regexp     []byte
	pos, depth int
}

func (c *globctx) compileExpression() error {
	for c.pos < len(c.glob) {
		switch curr := c.glob[c.pos]; curr {
		case '\\':
			if err := c.compileEscapeSequence(); err != nil {
				return err
			}
		case '*':
			if err := c.compileSubExpression("(?:", ")*", "[^/]*"); err != nil {
				return err
			}
		case '?':
			if err := c.compileSubExpression("(?:", ")?", "[^/]"); err != nil {
				return err
			}
		case '+':
			if err := c.compileSubExpression("(?:", ")+", "\\+"); err != nil {
				return err
			}
		case '@':
			if err := c.compileSubExpression("(?:", ")", "\\@"); err != nil {
				return err
			}
		case '!':
			if err := c.compileSubExpression("(?~", ")", "\\!"); err != nil {
				return err
			}
		case ')':
			if c.depth > 0 {
				return nil
			}
			c.regexp = append(c.regexp, "\\)"...)
			c.pos += 1

		case '|':
			if c.depth > 0 {
				c.regexp = append(c.regexp, '|')
				c.pos += 1
			} else {
				c.regexp = append(c.regexp, "\\|"...)
				c.pos += 1
			}
		case '/':
			if c.depth == 0 {
				if c.glob[c.pos:] == "/**" || strings.HasPrefix(c.glob[c.pos:], "/**/") {
					c.regexp = append(c.regexp, "(?:/.*)?"...)
					c.pos += 3
					break
				}
				if strings.HasPrefix(c.glob[c.pos:], "/:") {
					c.compileNamedCapture()
					break
				}
			}
			c.regexp = append(c.regexp, '/')
			c.pos += 1
		case '[':
			if err := c.compileCharacterClass(); err != nil {
				return err
			}
		case '.', '^', '$', '(', '{':
			c.regexp = append(c.regexp, '\\', curr)
			c.pos += 1
		default:
			c.regexp = append(c.regexp, curr)
			c.pos += 1
		}
	}

	if c.depth > 0 {
		return &syntax.Error{Code: syntax.ErrMissingParen, Expr: c.glob}
	}
	return nil
}

func (c *globctx) compileSubExpression(prefix string, suffix string, noexpr string) error {
	if strings.HasPrefix(c.glob[c.pos+1:], "(") {
		c.regexp = append(c.regexp, prefix...)
		c.depth += 1
		c.pos += 2
		if err := c.compileExpression(); err != nil {
			return err
		}
		c.regexp = append(c.regexp, suffix...)
		c.depth -= 1
		c.pos += 1
	} else {
		c.regexp = append(c.regexp, noexpr...)
		c.pos += 1
	}
	return nil
}

func (c *globctx) compileCharacterClass() error {
	c.regexp = append(c.regexp, '[')
	c.pos += 1

	if c.pos < len(c.glob) {
		switch curr := c.glob[c.pos]; curr {
		case ']', '-':
			c.regexp = append(c.regexp, curr)
			c.pos += 1

		case '!', '^':
			c.regexp = append(c.regexp, '^')
			c.pos += 1

			if strings.HasPrefix(c.glob[c.pos:], "]") {
				c.regexp = append(c.regexp, ']')
				c.pos += 1
			}
		}
	}

	for c.pos < len(c.glob) {
		if s := c.glob[c.pos:]; strings.HasPrefix(s, "[:") {
			if i := strings.Index(s[2:], ":]"); i >= 0 {
				c.regexp = append(c.regexp, s[:4+i]...)
				c.pos += 4 + i
				continue
			}
		}

		switch curr := c.glob[c.pos]; curr {
		case '\\':
			if err := c.compileEscapeSequence(); err != nil {
				return err
			}
		case ']':
			c.regexp = append(c.regexp, ']')
			c.pos += 1
			return nil
		default:
			c.regexp = append(c.regexp, curr)
			c.pos += 1
		}
	}

	return &syntax.Error{Code: syntax.ErrMissingBracket, Expr: c.glob}
}

func (c *globctx) compileEscapeSequence() error {
	if c.pos+1 == len(c.glob) {
		return &syntax.Error{Code: syntax.ErrTrailingBackslash, Expr: c.glob}
	}
	c.regexp = append(c.regexp, '\\', c.glob[c.pos+1])
	c.pos += 2
	return nil
}

func (c *globctx) compileGlobstarPrefix() {
	for strings.HasPrefix(c.glob[c.pos:], "**/") {
		c.regexp = append(c.regexp, "(?:[^/].*/)?"...)
		c.pos += 3
	}
	if c.glob[c.pos:] == "**" {
		c.regexp = append(c.regexp, "(?:[^/].*)?"...)
		c.pos += 2
	}
}

func (c *globctx) compileNamedCapture() {
	c.regexp = append(c.regexp, "(?:/(?P<"...)
	c.pos += 2

	for c.pos < len(c.glob) {
		switch curr := c.glob[c.pos]; curr {
		case '/':
			c.regexp = append(c.regexp, ">[^/]+))"...)
			return
		case '?':
			c.regexp = append(c.regexp, ">[^/]*))?"...)
			c.pos += 1
			return
		case '+':
			c.regexp = append(c.regexp, ">.+))"...)
			c.pos += 1
			return
		case '*':
			c.regexp = append(c.regexp, ">.*))?"...)
			c.pos += 1
			return
		case '>':
			c.regexp = append(c.regexp, '\\', curr)
			c.pos += 1
		default:
			c.regexp = append(c.regexp, curr)
			c.pos += 1
		}
	}

	c.regexp = append(c.regexp, ">[^/]+))"...)
}

func CompileTemplate(template string) string {
	var result []byte
	var pos int

	for pos < len(template) {
		switch curr := template[pos]; curr {
		case ':':
			if pos == 0 || template[pos-1] == '/' {
				result = append(result, "${"...)
				pos += 1

			loop:
				for pos < len(template) {
					switch curr := template[pos]; curr {
					case '/':
						break loop
					case '?', '+', '*':
						pos += 1
						break loop
					default:
						result = append(result, curr)
						pos += 1
					}
				}

				result = append(result, '}')
			} else {
				result = append(result, ':')
				pos += 1
			}
		case '$':
			result = append(result, "$$"...)
			pos += 1
		default:
			result = append(result, curr)
			pos += 1
		}
	}

	return string(result)
}
