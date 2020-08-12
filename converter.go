package tailwind

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/tdewolff/parse/v2/css"
)

// TODO: types that are:
// useful for output
// useful for filtering (callback func to veto rules from being output for smaller file size)
// can represent tree (so you can look and see that a rule is nested in a media query)
// workable with stream processing (do whatever extent possible)
// do not directly expose tdewolf css types (but can depend on them and include them unexported)

// New returns an initialized instance of Converter.  The out param
// indicates where output is written, it must not be nil.
func New(out io.Writer, dist Dist) *Converter {
	if out == nil {
		panic(fmt.Errorf("tailwind.Converter.out is nil, cannot continue"))
	}
	return &Converter{
		out:  out,
		dist: dist,
	}
}

// Converter does processing of CSS input files and writes a single output
// CSS file with the appropriate @ directives processed.
// Inputs are processed in the order they are added (see e.g. AddReader()).
type Converter struct {
	out          io.Writer
	inputs       []*input
	dist         Dist // tailwind is sourced from here
	*applier          // initialized as needed
	postProcFunc func(out io.Writer, in io.Reader) error
}

type input struct {
	name     string    // display file name
	r        io.Reader // read input from here
	isInline bool
}

// TODO: compare to https://tailwindcss.com/docs/controlling-file-size
// func (c *Converter) SetAllow(rule ...string) {
// 	panic(fmt.Errorf("not yet implemented"))
// }
// func (c *Converter) SetDisallow(rule ...string) {
// 	panic(fmt.Errorf("not yet implemented"))
// }

// SetPostProcFunc sets the function that is called to post-process the output of the converter.
// The typical use of this is for minification.
func (c *Converter) SetPostProcFunc(f func(out io.Writer, in io.Reader) error) {
	c.postProcFunc = f
}

// AddReader adds an input source. The name is used only in error
// messages to indicate the source. And r is the CSS source to be processed,
// it must not be nil.  If isInline it indicates this CSS is from an HTML
// style attribute, otherwise it's from the contents of a style tag or a
// standlone CSS file.
func (c *Converter) AddReader(name string, r io.Reader, isInline bool) {
	if r == nil {
		panic(fmt.Errorf("tailwind.Converter.AddReader(%q, r): r is nil, cannot continue", name))
	}
	c.inputs = append(c.inputs, &input{name: name, r: r, isInline: isInline})
}

// Run performs the conversion.  The output is written to the writer specified
// in New().
func (c *Converter) Run() (reterr error) {

	if c.out == nil {
		panic(fmt.Errorf("tailwind.Converter.out is nil, cannot continue"))
	}

	defer func() {
		if r := recover(); r != nil {
			e, ok := r.(error)
			if ok {
				reterr = e
			} else {
				reterr = fmt.Errorf("%v", r)
			}
		}
	}()

	// w := bufio.NewWriter(c.out)
	// defer func() { // ensure we always flush, and record the error if no other
	// 	err := w.Flush()
	// 	if err != nil && reterr == nil {
	// 		reterr = err
	// 	}
	// }()

	var w io.Writer = c.out

	// if postProcFunc is specified then use a pipe to integrate it
	if c.postProcFunc != nil {
		pr, pw := io.Pipe()
		w = pw
		var wg sync.WaitGroup
		wg.Add(1)
		defer wg.Wait()
		defer pw.Close()

		go func() {
			defer wg.Done()
			err := c.postProcFunc(c.out, pr)
			if err != nil && reterr == nil {
				reterr = err
			}
		}()
	}

	for _, in := range c.inputs {
		p := css.NewParser(in.r, in.isInline)

		err := c.runParse(in.name, p, w)
		if err != nil {
			return err
		}

	}

	return nil
}

func (c *Converter) runParse(name string, p *css.Parser, w io.Writer) error {

	for {

		gt, tt, data := p.Next()
		_ = tt

		// TODO: it's unfortunate we cannot get some sort of context from p,
		// although in the ErrorGrammar it does give it's own line number;
		// so for now we just print the name in front of every error

		switch gt {

		case css.ErrorGrammar:
			err := p.Err()
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("%s: %w", name, err)

		case css.AtRuleGrammar:

			switch {

			case bytes.Equal(data, []byte("@tailwind")):
				tokens := trimTokenWs(p.Values())
				if len(tokens) != 1 {
					return fmt.Errorf("%s: @tailwind should be followed by exactly one token, instead found: %v", name, tokens)
				}
				token := tokens[0]
				if token.TokenType != css.IdentToken {
					return fmt.Errorf("%s: @tailwind should be followed by an identifier token, instead found: %v", name, token)
				}
				switch string(token.Data) {
				case "base":

					rc, err := c.dist.OpenDist("base")
					if err != nil {
						return err
					}
					defer rc.Close()

					subp := css.NewParser(rc, false)
					err = c.runParse("[tailwind-dist/base]", subp, w)
					if err != nil {
						return err
					}

				case "components":

					rc, err := c.dist.OpenDist("components")
					if err != nil {
						return err
					}
					defer rc.Close()

					subp := css.NewParser(rc, false)
					err = c.runParse("[tailwind-dist/components]", subp, w)
					if err != nil {
						return err
					}

				case "utilities":

					rc, err := c.dist.OpenDist("utilities")
					if err != nil {
						return err
					}
					defer rc.Close()

					subp := css.NewParser(rc, false)
					err = c.runParse("[tailwind-dist/utilities]", subp, w)
					if err != nil {
						return err
					}

				default:
					return fmt.Errorf("%s: @tailwind followed by unknown identifier: %s", name, token.Data)
				}

			case bytes.Equal(data, []byte("@apply")):

				if c.applier == nil {
					var err error
					c.applier, err = newApplier(c.dist)
					if err != nil {
						return fmt.Errorf("error while creating applier: %w", err)
					}
				}

				idents, err := tokensToIdents(p.Values())
				if err != nil {
					return err
				}

				b, err := c.applier.apply(idents)
				if err != nil {
					return err
				}

				_, err = w.Write(b)
				if err != nil {
					return err
				}

			default: // other @ rules just get copied verbatim
				err := write(w, data, p.Values(), ';')
				if err != nil {
					return err
				}

			}

		case css.BeginAtRuleGrammar:
			err := write(w, data, p.Values(), '{')
			if err != nil {
				return err
			}

		case css.BeginRulesetGrammar:

			err := write(w, data, p.Values(), '{')
			if err != nil {
				return err
			}

		case css.DeclarationGrammar:
			err := write(w, data, ':', p.Values(), ';')
			if err != nil {
				return err
			}

		case css.QualifiedRuleGrammar:
			// NOTE: this is used for rules like: b,strong { ...
			// we'll get a QualifiedRuleGrammar entry with empty data and p.Values()
			// has the 'b' in it.
			err := write(w, p.Values(), ',')
			if err != nil {
				return err
			}

		case css.CustomPropertyGrammar:
			err := write(w, data, ':', p.Values(), ';')
			if err != nil {
				return err
			}

		case css.TokenGrammar:
			continue // just skip

		case css.CommentGrammar:
			continue // strip comments

		case css.EndRulesetGrammar, css.EndAtRuleGrammar:
			err := write(w, data)
			if err != nil {
				return err
			}

		default: // verify we aren't missing a type
			panic(fmt.Errorf("%s: unexpected grammar type %v at offset %v", name, gt, p.Offset()))

		}

	}

}

// scan tokens and extract just class names
func toklistClasses(toklist []css.Token) (ret []string) { // FIXME: think about efficiency - we should probably be using []byte and then have a static list of strings for stuff that can be @apply'd and only one copy of each of those strings
	priorDot := false
	for _, tok := range toklist {
		if tok.TokenType == css.DelimToken && bytes.Equal(tok.Data, []byte(".")) {
			priorDot = true
			continue
		}
		if priorDot && tok.TokenType == css.IdentToken {
			// parser will give us escapes and colons as part of identifiers, which indicate entires we can skip over for our purposes here
			if bytes.IndexByte(tok.Data, '\\') < 0 &&
				bytes.IndexByte(tok.Data, ':') < 0 {
				ret = append(ret, string(tok.Data))
			}
		}
		priorDot = false
	}
	return
}

// a general purpose write so we can just do one error check,
// check later for performance implications of interface{}
// and fmt.Fprintf here but I suspect it'll be minimal
func write(w io.Writer, what ...interface{}) error {
	for _, i := range what {

		switch v := i.(type) {

		case byte:
			fmt.Fprintf(w, "%c", v)

		case rune:
			fmt.Fprintf(w, "%c", v)

		case []byte:
			fmt.Fprintf(w, "%s", v)

		case []css.Token:
			err := writeTokens(w, v...)
			if err != nil {
				return err
			}

		default:
			_, err := fmt.Fprint(w, v)
			if err != nil {
				return err
			}
		}

	}
	return nil
}

func writeTokens(w io.Writer, tokens ...css.Token) error {
	for _, val := range tokens {
		_, err := w.Write(val.Data)
		if err != nil {
			return err
		}
	}
	return nil
}

func trimTokenWs(tokens []css.Token) []css.Token {
	for len(tokens) > 0 && tokens[0].TokenType == css.WhitespaceToken {
		tokens = tokens[1:]
	}
	for len(tokens) > 0 && tokens[len(tokens)-1].TokenType == css.WhitespaceToken {
		tokens = tokens[:len(tokens)-1]
	}
	return tokens
}

func tokensToIdents(tokens []css.Token) ([]string, error) {

	ret := make([]string, 0, len(tokens)/2)

	for _, token := range tokens {
		switch token.TokenType {
		case css.IdentToken:
			ret = append(ret, string(token.Data))
		case css.CommentToken, css.WhitespaceToken:
			// ignore
		default:
			return ret, fmt.Errorf("unexpected token while looking for ident: %v", token)
		}
	}

	return ret, nil
}