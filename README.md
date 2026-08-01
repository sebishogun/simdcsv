# simdcsv

**A CSV reader for Go that finds every delimiter at once instead of one byte at
a time.** Built on [simd.go](https://github.com/sebishogun/simd). No cgo.

```
go get github.com/sebishogun/simdcsv
```

```go
r := simdcsv.NewReader(f)
r.ReuseRecord = true

for {
	rec, err := r.Read()
	if err == io.EOF {
		break
	}
	name := rec.Field(0)   // []byte into the input, no copy
	...
}
```

The shape is `encoding/csv`'s. `Comma`, `FieldsPerRecord` and `ReuseRecord` mean
what they mean there, and every input this package accepts produces the same
records the standard library produces — there is a differential test over 400
randomised inputs that says so.

## Numbers

Zen 5, 20,000 rows, `ReuseRecord` on both sides, worse of two runs:

| | encoding/csv | simdcsv | |
|---|---|---|---|
| 16 columns, unquoted | 3.54 ms | 1.84 ms | **1.92×** |
| 4 columns, unquoted | 1.06 ms | 0.71 ms | **1.49×** |
| 16 columns, quoted | 3.74 ms | 3.50 ms | 1.07× |
| 4 columns, quoted | 1.10 ms | 1.35 ms | **0.81×** |

**The last row is the point.** A file where every field is quoted is about 20%
*slower* than the standard library, and there is no way around it that was worth
having — see below.

## Why unquoted is faster

`encoding/csv` walks the input a byte at a time, deciding at each one whether it
is a delimiter, a quote, a newline or data. That is a dependent branch per byte.

This finds them all at once. One vector compare writes the position of every
delimiter in a record, and the fields are then subslices between those
positions — no branch per byte, and no copy per field, because a field points
into the input buffer rather than into a fresh string.

## Why quoted is not

A quote changes what the bytes after it mean: a comma inside a quoted field is
data, and so is a newline. So a quoted record has to be walked, and this package
is then doing the standard library's work with a layer on top.

Two ways out were tried and measured, and both made it worse:

- **Hand quote-heavy files to `encoding/csv` wholesale** — 0.38×. Its records
  are `[]string`, and copying them into `[][]byte` costs more than the parsing
  saved.
- **Skip the vector call for short spans**, since `simd.IndexByte` costs about
  1.4 ns to call and a quoted record is a sequence of short fields — 1.30× on
  the *unquoted* case it was supposed to leave alone.

Neither is in the code. The 0.81× is.

## The trade this package makes

**It reads the whole input into memory** on the first `Read`. Finding every
delimiter at once needs a buffer to find them in. For a stream too large to
hold, use `encoding/csv`.

**Fields are `[]byte`, not `string`.** That is where a large part of the win
comes from: `encoding/csv` must copy each field because its buffer is reused,
and here the caller decides. `Record.Strings()` copies if that is what you want.

**`Comma` is a `byte`, not a `rune`.** The vector scan compares bytes. A
multi-byte delimiter would need a substring search per record, which costs more
than it saves.

## Correctness

Every test compares against `encoding/csv` on the same input — it is the
definition of correct here. That covers the hand-written cases (embedded
newlines, doubled quotes, `\r\n`, ragged records, blank lines) and 400
randomised ones built from atoms chosen to collide: unterminated quotes, quoted
delimiters, empty fields.

There is also a test that the fast path *runs*, because a suite that passed by
taking the careful path everywhere would look identical to one that worked. It
checks that a field from an unquoted record aliases the reader's buffer.

```
go test ./...
```

## Status

Early, and measured on amd64 only. The `simd` package underneath is verified on
amd64 and arm64 NEON and under emulation elsewhere.


## The rest of the family

All built on [simd.go](https://github.com/sebishogun/simd), which generates its
kernels once from C and ships them as committed assembly for nine instruction
sets — so none of these needs cgo, and none is amd64-only.

| | |
|---|---|
| [**simd.go**](https://github.com/sebishogun/simd) | 463 operations over slices, bytes and text. The kernels everything else is built from. |
| [**simdblas**](https://github.com/sebishogun/simdblas) | A BLAS backend for gonum. One `blas64.Use` call and `mat`, `stat` and `optimize` run on it. |
| [**simdjson**](https://github.com/sebishogun/simdjson) | Structural-index JSON parsing. Faster than minio/simdjson-go, and not amd64-only. |
| [**simdvec**](https://github.com/sebishogun/simdvec) | Embedding search whose whole index scan is one matrix-vector product. |

## License

MIT — see [LICENSE](LICENSE). Depends on
[simd.go](https://github.com/sebishogun/simd) (MIT).
