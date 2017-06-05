package mpb

import (
	"fmt"
	"io"
	"math"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	rLeft = iota
	rFill
	rTip
	rEmpty
	rRight
)

const (
	formatLen = 5
	etaAlpha  = 0.25
)

type barFmtRunes [formatLen]rune
type barFmtBytes [formatLen][]byte

// Bar represents a progress Bar
type Bar struct {
	incrCh        chan incrReq
	completeReqCh chan struct{}
	done          chan struct{}
	inProgress    chan struct{}
	ops           chan func(*state)

	// following are used after (*Bar.done) is closed
	width int
	state state
}

// Statistics represents statistics of the progress bar.
// Cantains: Total, Current, TimeElapsed, TimePerItemEstimate
type Statistics struct {
	ID                  int
	Completed           bool
	Aborted             bool
	Total               int
	Current             int
	StartTime           time.Time
	TimeElapsed         time.Duration
	TimePerItemEstimate time.Duration
}

// Refil is a struct for b.IncrWithReFill
type refill struct {
	char rune
	till int
}

// Eta returns exponential-weighted-moving-average ETA estimator
func (s *Statistics) Eta() time.Duration {
	return time.Duration(s.Total-s.Current) * s.TimePerItemEstimate
}

type (
	incrReq struct {
		amount int64
		refill *refill
	}
	state struct {
		id             int
		width          int
		format         barFmtRunes
		etaAlpha       float64
		total          int
		current        int
		trimLeftSpace  bool
		trimRightSpace bool
		completed      bool
		aborted        bool
		startTime      time.Time
		timeElapsed    time.Duration
		blockStartTime time.Time
		timePerItem    time.Duration
		appendFuncs    []DecoratorFunc
		prependFuncs   []DecoratorFunc
		simpleSpinner  func() byte
		refill         *refill
		// flushed        chan struct{}
	}
)

func newBar(total int, wg *sync.WaitGroup, cancel <-chan struct{}, options ...BarOption) *Bar {
	s := state{
		total:    total,
		etaAlpha: etaAlpha,
	}

	if total <= 0 {
		s.simpleSpinner = getSpinner()
	}

	for _, opt := range options {
		opt(&s)
	}

	b := &Bar{
		incrCh:        make(chan incrReq),
		completeReqCh: make(chan struct{}),
		done:          make(chan struct{}),
		inProgress:    make(chan struct{}),
		ops:           make(chan func(*state)),
	}
	b.width = s.width

	go b.server(s, wg, cancel)
	return b
}

// RemoveAllPrependers removes all prepend functions
func (b *Bar) RemoveAllPrependers() {
	select {
	case b.ops <- func(s *state) {
		s.prependFuncs = nil
	}:
	case <-b.done:
		return
	}
}

// RemoveAllAppenders removes all append functions
func (b *Bar) RemoveAllAppenders() {
	select {
	case b.ops <- func(s *state) {
		s.appendFuncs = nil
	}:
	case <-b.done:
		return
	}
}

// ProxyReader wrapper for io operations, like io.Copy
func (b *Bar) ProxyReader(r io.Reader) *Reader {
	return &Reader{r, b}
}

// Incr increments progress bar
func (b *Bar) Incr(n int) {
	if n < 1 {
		return
	}
	select {
	case b.ops <- func(s *state) {
		if s.current == 0 {
			s.startTime = time.Now()
			s.blockStartTime = s.startTime
		}
		sum := s.current + n
		s.timeElapsed = time.Since(s.startTime)
		s.updateTimePerItemEstimate(n)
		if s.total > 0 && sum >= s.total {
			s.current = s.total
			s.completed = true
			return
		}
		s.current = sum
		s.blockStartTime = time.Now()
	}:
	case <-b.done:
		return
	}
}

// ResumeFill fills bar with different r rune,
// from 0 to till amount of progress.
func (b *Bar) ResumeFill(r rune, till int) {
	if till < 1 {
		return
	}
	select {
	case b.ops <- func(s *state) {
		s.refill = &refill{r, till}
	}:
	case <-b.done:
		return
	}
}

func (b *Bar) NumOfAppenders() int {
	result := make(chan int, 1)
	select {
	case b.ops <- func(s *state) { result <- len(s.appendFuncs) }:
		return <-result
	case <-b.done:
		return len(b.state.appendFuncs)
	}
}

func (b *Bar) NumOfPrependers() int {
	result := make(chan int, 1)
	select {
	case b.ops <- func(s *state) { result <- len(s.prependFuncs) }:
		return <-result
	case <-b.done:
		return len(b.state.prependFuncs)
	}
}

// Statistics returs *Statistics, which contains information like
// Tottal, Current, TimeElapsed and TimePerItemEstimate
func (b *Bar) Statistics() *Statistics {
	result := make(chan *Statistics, 1)
	select {
	case b.ops <- func(s *state) { result <- newStatistics(s) }:
		return <-result
	case <-b.done:
		return newStatistics(&b.state)
	}
}

// GetID returs id of the bar
func (b *Bar) GetID() int {
	result := make(chan int, 1)
	select {
	case b.ops <- func(s *state) { result <- s.id }:
		return <-result
	case <-b.done:
		return b.state.id
	}
}

// InProgress returns true, while progress is running.
// Can be used as condition in for loop
func (b *Bar) InProgress() bool {
	select {
	case <-b.completeReqCh:
		return false
	default:
		return true
	}
}

// Complete signals to the bar, that process has been completed.
// You should call this method when total is unknown and you've reached the point
// of process completion. If you don't call this method, it will be called
// implicitly, upon p.Stop() call.
func (b *Bar) Complete() {
	select {
	case <-b.completeReqCh:
		return
	default:
		close(b.completeReqCh)
	}
}

func (b *Bar) server(s state, wg *sync.WaitGroup, cancel <-chan struct{}) {

	defer func() {
		b.state = s
		// <-s.flushed
		// fmt.Fprintf(os.Stderr, "Bar:%d flushed\n", s.id)
		wg.Done()
		close(b.done)
	}()

	for {
		select {
		case op := <-b.ops:
			op(&s)
		case <-b.completeReqCh:
			s.completed = true
			return
		case <-cancel:
			s.aborted = true
			cancel = nil
			b.Complete()
		}
	}
}

// func (b *Bar) render(tw int, flushed chan struct{}, prependWs, appendWs *widthSync) <-chan []byte {
// 	ch := make(chan []byte)

// 	go func() {
// 		defer func() {
// 			// recovering if external decorators panic
// 			if p := recover(); p != nil {
// 				ch <- []byte(fmt.Sprintln(p))
// 			}
// 			close(ch)
// 		}()
// 		result := make(chan []byte, 1)
// 		select {
// 		case b.ops <- func(s *state) {
// 			buf := draw(s, tw, prependWs, appendWs)
// 			buf = append(buf, '\n')
// 			result <- buf
// 			// wait for flushed
// 			if s.completed {
// 				<-flushed
// 				b.Complete()
// 			}
// 		}:
// 			ch <- <-result
// 		case <-b.done:
// 			buf := draw(&b.state, tw, prependWs, appendWs)
// 			buf = append(buf, '\n')
// 			ch <- buf
// 		default:
// 			ch <- []byte{}
// 		}
// 	}()

// 	return ch
// }

func (b *Bar) render(tw int, flushed chan struct{}, prependWs, appendWs *widthSync) <-chan []byte {
	ch := make(chan []byte)

	go func() {
		defer func() {
			// recovering if external decorators panic
			if p := recover(); p != nil {
				ch <- []byte(fmt.Sprintln(p))
			}
			close(ch)
		}()
		var st state
		result := make(chan state, 1)
		select {
		case b.ops <- func(s *state) {
			result <- *s
			if s.completed {
				<-flushed
				b.Complete()
			}
		}:
			st = <-result
		case <-b.done:
			st = b.state
		}
		buf := draw(&st, tw, prependWs, appendWs)
		buf = append(buf, '\n')
		ch <- buf
	}()

	return ch
}

func (s *state) updateFormat(format string) {
	for i, n := 0, 0; len(format) > 0; i++ {
		s.format[i], n = utf8.DecodeRuneInString(format)
		format = format[n:]
	}
}

func (s *state) updateTimePerItemEstimate(amount int) {
	lastBlockTime := time.Since(s.blockStartTime) // shorthand for time.Now().Sub(t)
	lastItemEstimate := float64(lastBlockTime) / float64(amount)
	s.timePerItem = time.Duration((s.etaAlpha * lastItemEstimate) + (1-s.etaAlpha)*float64(s.timePerItem))
}

func draw(s *state, termWidth int, prependWs, appendWs *widthSync) []byte {
	if len(s.prependFuncs) != len(prependWs.listen) || len(s.appendFuncs) != len(appendWs.listen) {
		return []byte{}
	}
	if termWidth <= 0 {
		termWidth = s.width
	}

	stat := newStatistics(s)

	// render prepend functions to the left of the bar
	var prependBlock []byte
	for i, f := range s.prependFuncs {
		prependBlock = append(prependBlock,
			[]byte(f(stat, prependWs.listen[i], prependWs.result[i]))...)
	}

	// render append functions to the right of the bar
	var appendBlock []byte
	for i, f := range s.appendFuncs {
		appendBlock = append(appendBlock,
			[]byte(f(stat, appendWs.listen[i], appendWs.result[i]))...)
	}

	prependCount := utf8.RuneCount(prependBlock)
	appendCount := utf8.RuneCount(appendBlock)

	var leftSpace, rightSpace []byte
	space := []byte{' '}

	if !s.trimLeftSpace {
		prependCount++
		leftSpace = space
	}
	if !s.trimRightSpace {
		appendCount++
		rightSpace = space
	}

	var barBlock []byte
	buf := make([]byte, 0, termWidth)
	fmtBytes := convertFmtRunesToBytes(s.format)

	if s.simpleSpinner != nil {
		for _, block := range [...][]byte{fmtBytes[rLeft], {s.simpleSpinner()}, fmtBytes[rRight]} {
			barBlock = append(barBlock, block...)
		}
		return concatenateBlocks(buf, prependBlock, leftSpace, barBlock, rightSpace, appendBlock)
	}

	barBlock = fillBar(s.total, s.current, s.width, fmtBytes, s.refill)
	barCount := utf8.RuneCount(barBlock)
	totalCount := prependCount + barCount + appendCount
	if totalCount > termWidth {
		newWidth := termWidth - prependCount - appendCount
		barBlock = fillBar(s.total, s.current, newWidth, fmtBytes, s.refill)
	}

	return concatenateBlocks(buf, prependBlock, leftSpace, barBlock, rightSpace, appendBlock)
}

func concatenateBlocks(buf []byte, blocks ...[]byte) []byte {
	for _, block := range blocks {
		buf = append(buf, block...)
	}
	return buf
}

func fillBar(total, current, width int, fmtBytes barFmtBytes, rf *refill) []byte {
	if width < 2 || total <= 0 {
		return []byte{}
	}

	// bar width without leftEnd and rightEnd runes
	barWidth := width - 2

	completedWidth := percentage(total, current, barWidth)

	buf := make([]byte, 0, width)
	buf = append(buf, fmtBytes[rLeft]...)

	if rf != nil {
		till := percentage(total, rf.till, barWidth)
		rbytes := make([]byte, utf8.RuneLen(rf.char))
		utf8.EncodeRune(rbytes, rf.char)
		// append refill rune
		for i := 0; i < till; i++ {
			buf = append(buf, rbytes...)
		}
		for i := till; i < completedWidth; i++ {
			buf = append(buf, fmtBytes[rFill]...)
		}
	} else {
		for i := 0; i < completedWidth; i++ {
			buf = append(buf, fmtBytes[rFill]...)
		}
	}

	if completedWidth < barWidth && completedWidth > 0 {
		_, size := utf8.DecodeLastRune(buf)
		buf = buf[:len(buf)-size]
		buf = append(buf, fmtBytes[rTip]...)
	}

	for i := completedWidth; i < barWidth; i++ {
		buf = append(buf, fmtBytes[rEmpty]...)
	}

	buf = append(buf, fmtBytes[rRight]...)

	return buf
}

func newStatistics(s *state) *Statistics {
	return &Statistics{
		ID:                  s.id,
		Completed:           s.completed,
		Aborted:             s.aborted,
		Total:               s.total,
		Current:             s.current,
		StartTime:           s.startTime,
		TimeElapsed:         s.timeElapsed,
		TimePerItemEstimate: s.timePerItem,
	}
}

func convertFmtRunesToBytes(format barFmtRunes) barFmtBytes {
	var fmtBytes barFmtBytes
	for i, r := range format {
		buf := make([]byte, utf8.RuneLen(r))
		utf8.EncodeRune(buf, r)
		fmtBytes[i] = buf
	}
	return fmtBytes
}

func percentage(total, current, ratio int) int {
	if total == 0 || current > total {
		return 0
	}
	num := float64(ratio) * float64(current) / float64(total)
	ceil := math.Ceil(num)
	diff := ceil - num
	// num = 2.34 will return 2
	// num = 2.44 will return 3
	if math.Max(diff, 0.6) == diff {
		return int(num)
	}
	return int(ceil)
}

func getSpinner() func() byte {
	chars := []byte(`-\|/`)
	repeat := len(chars) - 1
	index := repeat
	return func() byte {
		if index == repeat {
			index = -1
		}
		index++
		return chars[index]
	}
}
