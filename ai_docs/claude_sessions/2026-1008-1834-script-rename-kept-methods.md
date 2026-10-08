# Script F2: keep methods that outside code calls by name

Session: `4d5b517c-83dd-4ee5-98cc-e3ef6fc53926`

## Ask

1. Dropped in from the cats-todo backlog: N-167. Script F2 could rename a
   method that satisfies an interface only an import knows (`String()` for
   `fmt.Stringer`). Imports are stand-ins, so the recheck saw no error, and
   `s.Print(v)` silently stopped calling it. Refuse, or warn on, a method
   whose name is a well-known interface's.
2. `/sw` (sess-wrap).

## Decisions

- **Refuse, not warn.** A refusal fits F2's existing contract: a rename
  that passes can only have renamed. Renaming away is refused up front, as
  `Symbol.Fixed`, so the rename box does not open and says why. That needed
  no UI change.
- **Both directions.** Renaming `str() string` *to* `String` makes fmt start
  calling it, which changes what the script does just as silently. So
  `Rename` also refuses a new name that, with the method's signature, is a
  well-known one. If this proves annoying (renaming to `String` is often the
  intent), it is two lines in `Rename` to drop.
- **Name and signature, not name alone.** `String(w int) string` and
  `Len() int64` satisfy nothing, so they still rename. Under stand-in imports
  a parameter like `fmt.State` has an invalid type, so the signature is read
  from the declaration's syntax and spelled one way: imported types by their
  package's own name (`f.State` under `import f "fmt"` is `fmt.State`),
  `uint8`→`byte`, `int32`→`rune`, `interface{}`→`any`, and a script-declared
  type as `main.T`, so a script's own `string` never matches Go's.
- **Concrete receivers only.** An interface's own method is not refused:
  renaming it changes which types satisfy that interface, which the recheck
  sees wherever the script uses the interface.
- **`Error()` is on the list.** The item called it safe, but the recheck
  catches the break only where the script uses the value as an `error`. A
  type with `Error() string` that only reaches `s.Print` breaks just as
  silently as `String()`.
- **The list:** fmt (`Error`, `String`, `GoString`, `Format`, `Scan`),
  json, text, binary and xml marshalers and unmarshalers, `sql.Scanner`,
  `driver.Valuer`, errors' `Unwrap`/`Is`/`As`, io's `Read`/`Write`/`Close`/
  `WriteTo`/`ReadFrom`, sort's `Len`/`Less`/`Swap`, heap's `Push`/`Pop`. It
  covers the common cases, not every interface.

## Changes

- `script/rename_iface.go` (new): `wellKnownMethods` table;
  `checked.keptMethod(obj, name)` (name == obj's own name asks about the
  rename away, any other the rename to); `funcType`, `sigText`, `typeText`
  to spell a declared signature. Doc block WHY SOME METHOD NAMES ARE KEPT.
- `script/rename.go`: `fixed` calls `keptMethod` for a `*types.Func`;
  `Rename` calls it with the new name after `taken`; WHAT IS REFUSED UP
  FRONT mentions it.
- `script/rename_test.go`: six refusal cases in `TestRenameRefused`
  (`String`, `Error` never used as an error, `Format` under a renamed fmt
  import with `int32`, `driver.Valuer`, grouped `Less(i, j int)`, renaming
  to `String`). New `TestRenameKeptMethodNot` has five that must still
  rename: another signature, `Len() int64`, a script-declared `string`, an
  interface's own `String`, and renaming to `String` with another signature.

## Verification

- `go test ./...` (CATS_* stripped) passes, and so do `go vet` and `gofmt`.
- With `keptMethod` short-circuited, all six new refusal cases fail.
- Not checked in the browser. The box shows `Symbol.Fixed` the same way
  for every refusal reason, so no web or e2e change was needed.

## Next

Closed: N-167. Declined: None. Raised: None.
Deferred: None. Promoted: None. Moved: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
