// Package reversi implements the Misskey reversi (Othello) game logic.
// misskey-reversi パッケージの Go 移植。
package reversi

import (
	"encoding/json"
	"hash/crc32"
)

// Color represents a stone color. true=Black, false=White.
type Color = bool

const (
	Black Color = true
	White Color = false
)

// MapCell represents a cell type on the board.
type MapCell int

const (
	CellNull  MapCell = iota // 配置不可能
	CellEmpty                // 配置可能 (空)
)

// Options holds game rule variations.
type Options struct {
	IsLlotheo        bool `json:"isLlotheo"`
	CanPutEverywhere bool `json:"canPutEverywhere"`
	LoopedBoard      bool `json:"loopedBoard"`
}

// Undo records a single move for undo support.
type Undo struct {
	Color   Color  `json:"color"`
	Pos     int    `json:"pos"`
	Effects []int  `json:"effects"`
	Turn    *Color `json:"turn"`
}

// Game manages the reversi board state.
type Game struct {
	BoardMap  []MapCell
	Board     []*bool // nil=empty, &true=black, &false=white
	MapWidth  int
	MapHeight int
	Turn      *Color // nil = game ended
	Opts      Options
	PrevPos   int
	PrevColor *Color
	Logs      []Undo
}

// NewGame creates a game from a map definition.
// mapData is a slice of strings where each character is:
//   - '-' = empty cell
//   - 'b' = black stone
//   - 'w' = white stone
//   - anything else = null (not playable)
func NewGame(mapData []string, opts Options) *Game {
	if len(mapData) == 0 {
		mapData = []string{"--------", "--------", "--------", "---wb---", "---bw---", "--------", "--------", "--------"}
	}
	width := len(mapData[0])
	height := len(mapData)
	flat := ""
	for _, row := range mapData {
		flat += row
	}

	board := make([]*bool, len(flat))
	boardMap := make([]MapCell, len(flat))
	for i, ch := range flat {
		switch ch {
		case '-':
			board[i] = nil
			boardMap[i] = CellEmpty
		case 'b':
			b := Black
			board[i] = &b
			boardMap[i] = CellEmpty
		case 'w':
			w := White
			board[i] = &w
			boardMap[i] = CellEmpty
		default:
			board[i] = nil
			boardMap[i] = CellNull
		}
	}

	g := &Game{
		BoardMap:  boardMap,
		Board:     board,
		MapWidth:  width,
		MapHeight: height,
		Opts:      opts,
		PrevPos:   -1,
		Logs:      make([]Undo, 0),
	}

	// 初期ターン計算
	black := Black
	g.Turn = &black
	if !g.CanPutSomewhere(Black) {
		if g.CanPutSomewhere(White) {
			white := White
			g.Turn = &white
		} else {
			g.Turn = nil
		}
	}

	return g
}

// PosToXY converts a flat position to (x, y).
func (g *Game) PosToXY(pos int) (int, int) {
	return pos % g.MapWidth, pos / g.MapWidth
}

// XYToPos converts (x, y) to a flat position.
func (g *Game) XYToPos(x, y int) int {
	return x + y*g.MapWidth
}

// BlackCount returns the number of black stones.
func (g *Game) BlackCount() int {
	count := 0
	for _, s := range g.Board {
		if s != nil && *s == Black {
			count++
		}
	}
	return count
}

// WhiteCount returns the number of white stones.
func (g *Game) WhiteCount() int {
	count := 0
	for _, s := range g.Board {
		if s != nil && *s == White {
			count++
		}
	}
	return count
}

// PutStone places a stone at the given position.
func (g *Game) PutStone(pos int) {
	if g.Turn == nil {
		return
	}
	// 盤面の外は置かない (記録の再生で壊れた値が来ても panic しない)。
	if pos < 0 || pos >= len(g.Board) {
		return
	}
	color := *g.Turn
	g.PrevPos = pos
	g.PrevColor = &color
	g.Board[pos] = &color

	effects := g.Effects(color, pos)
	for _, p := range effects {
		c := color
		g.Board[p] = &c
	}

	turn := *g.Turn
	g.Logs = append(g.Logs, Undo{
		Color:   color,
		Pos:     pos,
		Effects: effects,
		Turn:    &turn,
	})

	g.calcTurn()
}

func (g *Game) calcTurn() {
	if g.PrevColor == nil {
		return
	}
	opposite := !*g.PrevColor
	if g.CanPutSomewhere(opposite) {
		g.Turn = &opposite
	} else if g.CanPutSomewhere(*g.PrevColor) {
		g.Turn = g.PrevColor
	} else {
		g.Turn = nil
	}
}

// MapDataGet returns the cell type at a position.
func (g *Game) MapDataGet(pos int) MapCell {
	x, y := g.PosToXY(pos)
	if x < 0 || y < 0 || x >= g.MapWidth || y >= g.MapHeight || pos < 0 || pos >= len(g.BoardMap) {
		return CellNull
	}
	return g.BoardMap[pos]
}

// GetPuttablePlaces returns all positions where a stone can be placed.
func (g *Game) GetPuttablePlaces(color Color) []int {
	var places []int
	for i := range g.Board {
		if g.CanPut(color, i) {
			places = append(places, i)
		}
	}
	return places
}

// CanPutSomewhere checks if a color can place a stone anywhere.
func (g *Game) CanPutSomewhere(color Color) bool {
	return len(g.GetPuttablePlaces(color)) > 0
}

// CanPut checks if a stone can be placed at the given position.
func (g *Game) CanPut(color Color, pos int) bool {
	if pos < 0 || pos >= len(g.Board) {
		return false
	}
	if g.Board[pos] != nil {
		return false // 既に石がある
	}
	if g.Opts.CanPutEverywhere {
		return g.MapDataGet(pos) == CellEmpty
	}
	return len(g.Effects(color, pos)) > 0
}

// Effects returns the positions that would be flipped by placing a stone.
func (g *Game) Effects(color Color, initPos int) []int {
	enemy := !color
	diffs := [][2]int{
		{0, -1}, {1, -1}, {1, 0}, {1, 1},
		{0, 1}, {-1, 1}, {-1, 0}, {-1, -1},
	}

	var all []int
	for _, d := range diffs {
		all = append(all, g.effectsInLine(color, enemy, initPos, d[0], d[1])...)
	}
	return all
}

func (g *Game) effectsInLine(color, enemy Color, initPos, dx, dy int) []int {
	var found []int
	x, y := g.PosToXY(initPos)
	for {
		x += dx
		y += dy

		if g.Opts.LoopedBoard {
			x = ((x % g.MapWidth) + g.MapWidth) % g.MapWidth
			y = ((y % g.MapHeight) + g.MapHeight) % g.MapHeight
			if g.XYToPos(x, y) == initPos {
				return found
			}
		} else if x < 0 || y < 0 || x >= g.MapWidth || y >= g.MapHeight {
			return nil
		}

		pos := g.XYToPos(x, y)
		if g.MapDataGet(pos) == CellNull {
			return nil
		}
		stone := g.Board[pos]
		if stone == nil {
			return nil
		}
		if *stone == enemy {
			found = append(found, pos)
		}
		if *stone == color {
			return found
		}
	}
}

// CalcCRC32 computes the CRC32 hash of the board state.
func (g *Game) CalcCRC32() uint32 {
	data, _ := json.Marshal(map[string]any{
		"board": g.Board,
		"turn":  g.Turn,
	})
	return crc32.ChecksumIEEE(data)
}

// IsEnded returns true if the game has ended.
func (g *Game) IsEnded() bool {
	return g.Turn == nil
}

// Winner returns the winning color, or nil for a draw.
// Only valid when IsEnded() is true.
func (g *Game) Winner() *Color {
	if !g.IsEnded() {
		return nil
	}
	bc := g.BlackCount()
	wc := g.WhiteCount()
	if bc == wc {
		return nil // 引き分け
	}
	// isLlotheo: 石が少ない方が勝ち
	var winner Color
	if g.Opts.IsLlotheo {
		if bc < wc {
			winner = Black
		} else {
			winner = White
		}
	} else {
		if bc > wc {
			winner = Black
		} else {
			winner = White
		}
	}
	return &winner
}

// GetLogs returns the game logs.
func (g *Game) GetLogs() [][]int {
	out := make([][]int, len(g.Logs))
	for i, log := range g.Logs {
		colorInt := 0
		if log.Color {
			colorInt = 1
		}
		out[i] = []int{log.Pos, colorInt}
	}
	return out
}
