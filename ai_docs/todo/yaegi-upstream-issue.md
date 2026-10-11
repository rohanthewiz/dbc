# Draft: yaegi issue — an operator's result stored into a compiled [][]any's element is mis-stored

For N-188. A report ready to file at https://github.com/traefik/yaegi/issues
(the user's to post; dbc's own copy of the repro is
`script.TestYaegiStoreComputedBug`). Written 2026-10-10 against yaegi
v0.16.1. The master build fcb76d1 (2026-02-09) still reproduces it, per
the 2026-10-10 `/next-list` check.

---

**Title:** Assigning a binary expression into an element of a `[][]any` passed from compiled code returns a nil result and leaves the element unchanged

**yaegi version:** v0.16.1 (also master fcb76d1, 2026-02-09)
**Go version:** go1.25.4 and go1.26.1, darwin/arm64

### What happens

Compiled code hands an interpreted function a `*Batch` whose field is
`Rows [][]any`. The interpreted function assigns the result of an
operator straight into an element:

```go
b.Rows[i][1] = b.Rows[i][1].(string) + "!"
```

The element keeps its old value. A local of the function appears to be
overwritten instead: the `return b, nil` that follows returns a **nil**
`*Batch`. The same store through a variable works:

```go
s := b.Rows[i][1].(string) + "!"
b.Rows[i][1] = s
```

Calls, comparisons, plain variables and `any(…)` on the right-hand side
are fine. So is a `[][]any` the interpreted code made itself. In dbc's
use the same happens through `row := b.Rows[i]; row[c] = s + "!"`.

### Repro

`go.mod`: `require github.com/traefik/yaegi v0.16.1`

```go
// Minimal repro: an operator's result assigned into an element of a
// [][]any that compiled code handed to interpreted code is mis-stored.
package main

import (
	"fmt"
	"reflect"

	"github.com/traefik/yaegi/interp"
	"github.com/traefik/yaegi/stdlib"
)

// Batch is the compiled type handed to the interpreted function.
type Batch struct{ Rows [][]any }

const src = `package p

import "repro"

func Apply(b *repro.Batch) (*repro.Batch, error) {
	for i := range b.Rows {
		b.Rows[i][1] = b.Rows[i][1].(string) + "!"
	}
	return b, nil
}

// the same store through a variable: works
func ApplyVar(b *repro.Batch) (*repro.Batch, error) {
	for i := range b.Rows {
		s := b.Rows[i][1].(string) + "!"
		b.Rows[i][1] = s
	}
	return b, nil
}
`

func main() {
	i := interp.New(interp.Options{})
	if err := i.Use(stdlib.Symbols); err != nil {
		panic(err)
	}
	if err := i.Use(interp.Exports{"repro/repro": {"Batch": reflect.ValueOf((*Batch)(nil))}}); err != nil {
		panic(err)
	}
	if _, err := i.Eval(src); err != nil {
		panic(err)
	}
	v, err := i.Eval("p.Apply")
	if err != nil {
		panic(err)
	}
	apply := v.Interface().(func(*Batch) (*Batch, error))
	in := &Batch{Rows: [][]any{{int64(1), "a"}, {int64(2), "b"}}}
	out, err := apply(in)
	fmt.Printf("returned batch: %v, err: %v\n", out, err)
	fmt.Printf("rows after: %v\n", in.Rows)
	fmt.Println("expected: returned batch &{[[1 a!] [2 b!]]}, rows [[1 a!] [2 b!]]")

	v, _ = i.Eval("p.ApplyVar")
	in = &Batch{Rows: [][]any{{int64(1), "a"}, {int64(2), "b"}}}
	out, err = v.Interface().(func(*Batch) (*Batch, error))(in)
	fmt.Printf("control (through a variable): returned %v, err %v\n", out, err)
}
```

Output:

```
returned batch: <nil>, err: <nil>
rows after: [[1 a] [2 b]]
expected: returned batch &{[[1 a!] [2 b!]]}, rows [[1 a!] [2 b!]]
control (through a variable): returned &{[[1 a!] [2 b!]]}, err <nil>
```

### Expected

`Apply` returns the batch it was given, with each row's second value
suffixed, as `ApplyVar` does and as the same code compiled by Go does.
