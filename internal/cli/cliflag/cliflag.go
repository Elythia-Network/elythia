// Package cliflag holds the flag handling shared by the elythia subcommands.
package cliflag

import (
	"errors"
	"flag"
	"fmt"
	"io"
)

// New returns a FlagSet for "elythia <name>" that reports parse errors to the
// caller instead of exiting the process, and writes its messages to w.
//
// グローバルの flag.CommandLine は使わない。サブコマンドごとに flag の集合が
// 違うので、1 つの集合に全部を登録すると `elythia serve -fix` のような組み合わせが
// エラーにならずに通ってしまう。ExitOnError にしないのは、終了コードを呼び出し側
// (dispatcher) に返してテストから観測できるようにするため。
func New(name string, w io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet("elythia "+name, flag.ContinueOnError)
	fs.SetOutput(w)
	fs.Usage = func() {
		fmt.Fprintf(w, "Usage: elythia %s [flags]\n\nFlags:\n", name)
		fs.PrintDefaults()
	}
	return fs
}

// Parse parses args into fs. When ok is false the subcommand must stop and
// return code: 0 after -h / -help, 2 after a bad flag or a stray positional
// argument (the same codes flag.ExitOnError uses).
//
// **余った位置引数は拒否する。** 以前の各バイナリは flag.Parse の後ろを見ておらず、
// `-dry-run` を `dry-run` と書き間違えると黙って無視して書き込みまで進んだ。
// サブコマンドにまとめると引数の位置がずれやすくなるので、ここで止める。
func Parse(fs *flag.FlagSet, args []string) (code int, ok bool) {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0, false
		}
		// エラーの文面と usage は flag パッケージが fs.Output() に書き済み。
		return 2, false
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(fs.Output(), "%s: unexpected argument %q\n", fs.Name(), fs.Arg(0))
		fs.Usage()
		return 2, false
	}
	return 0, true
}
