package netstorage

import (
	"fmt"
	"net/http"

	"github.com/VictoriaMetrics/VictoriaMetrics/lib/auth"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/bytesutil"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/consts"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/encoding"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/httpserver"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/logger"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/prompb"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/storage"
	xxhash "github.com/cespare/xxhash/v2"
	jump "github.com/lithammer/go-jump-consistent-hash"
)

// InsertCtx is a generic context for inserting data.
//
// InsertCtx.Reset must be called before the first usage.
type InsertCtx struct {
	Labels        []prompb.Label
	MetricNameBuf []byte

	bufRowss  []bufRows
	labelsBuf []byte

	resultCh chan error
}

type bufRows struct {
	buf  []byte
	rows int
}

func (br *bufRows) pushTo(sn *storageNode) error {
	bufLen := len(br.buf)
	err := sn.push(br.buf, br.rows)
	br.buf = br.buf[:0]
	br.rows = 0
	if err != nil {
		return &httpserver.ErrorWithStatusCode{
			Err:        fmt.Errorf("cannot send %d bytes to storageNode %q: %s", bufLen, sn.dialer.Addr(), err),
			StatusCode: http.StatusServiceUnavailable,
		}
	}
	return nil
}

// Reset resets ctx.
func (ctx *InsertCtx) Reset() {
	for _, label := range ctx.Labels {
		label.Name = nil
		label.Value = nil
	}
	ctx.Labels = ctx.Labels[:0]
	ctx.MetricNameBuf = ctx.MetricNameBuf[:0]

	if ctx.bufRowss == nil {
		ctx.bufRowss = make([]bufRows, len(storageNodes))
	}
	for i := range ctx.bufRowss {
		br := &ctx.bufRowss[i]
		br.buf = br.buf[:0]
		br.rows = 0
	}
	ctx.labelsBuf = ctx.labelsBuf[:0]
	if ctx.resultCh == nil {
		ctx.resultCh = make(chan error, len(storageNodes))
	} else if len(ctx.resultCh) > 0 {
		logger.Panicf("BUG: ctx.resultCh must be empty on Reset; got %d items", len(ctx.resultCh))
	}
}

// AddLabelBytes adds (name, value) label to ctx.Labels.
//
// name and value must exist until ctx.Labels is used.
func (ctx *InsertCtx) AddLabelBytes(name, value []byte) {
	labels := ctx.Labels
	if cap(labels) > len(labels) {
		labels = labels[:len(labels)+1]
	} else {
		labels = append(labels, prompb.Label{})
	}
	label := &labels[len(labels)-1]

	// Do not copy name and value contents for performance reasons.
	// This reduces GC overhead on the number of objects and allocations.
	label.Name = name
	label.Value = value

	ctx.Labels = labels
}

// AddLabel adds (name, value) label to ctx.Labels.
//
// name and value must exist until ctx.Labels is used.
func (ctx *InsertCtx) AddLabel(name, value string) {
	labels := ctx.Labels
	if cap(labels) > len(labels) {
		labels = labels[:len(labels)+1]
	} else {
		labels = append(labels, prompb.Label{})
	}
	label := &labels[len(labels)-1]

	// Do not copy name and value contents for performance reasons.
	// This reduces GC overhead on the number of objects and allocations.
	label.Name = bytesutil.ToUnsafeBytes(name)
	label.Value = bytesutil.ToUnsafeBytes(value)

	ctx.Labels = labels
}

// WriteDataPoint writes (timestamp, value) data point with the given at and labels to ctx buffer.
func (ctx *InsertCtx) WriteDataPoint(at *auth.Token, labels []prompb.Label, timestamp int64, value float64) error {
	ctx.MetricNameBuf = storage.MarshalMetricNameRaw(ctx.MetricNameBuf[:0], at.AccountID, at.ProjectID, labels)
	storageNodeIdx := ctx.GetStorageNodeIdx(at, labels)
	return ctx.WriteDataPointExt(at, storageNodeIdx, ctx.MetricNameBuf, timestamp, value)
}

// WriteDataPointExt writes the given metricNameRaw with (timestmap, value) to ctx buffer with the given storageNodeIdx.
func (ctx *InsertCtx) WriteDataPointExt(at *auth.Token, storageNodeIdx int, metricNameRaw []byte, timestamp int64, value float64) error {
	br := &ctx.bufRowss[storageNodeIdx]
	sn := storageNodes[storageNodeIdx]
	bufNew := storage.MarshalMetricRow(br.buf, metricNameRaw, timestamp, value)
	if len(bufNew) >= consts.MaxInsertPacketSize {
		// Send buf to storageNode, since it is too big.
		if err := br.pushTo(sn); err != nil {
			return err
		}
		br.buf = storage.MarshalMetricRow(bufNew[:0], metricNameRaw, timestamp, value)
	} else {
		br.buf = bufNew
	}
	br.rows++
	return nil
}

// FlushBufs flushes ctx bufs to remote storage nodes.
func (ctx *InsertCtx) FlushBufs() error {
	// Send per-storageNode bufs in parallel.
	resultCh := ctx.resultCh
	resultChLen := 0
	for i := range ctx.bufRowss {
		br := &ctx.bufRowss[i]
		if len(br.buf) == 0 {
			continue
		}
		resultChLen++
		go func(br *bufRows, sn *storageNode) {
			resultCh <- br.pushTo(sn)
		}(br, storageNodes[i])
	}
	var lastErr error
	for i := 0; i < resultChLen; i++ {
		err := <-resultCh
		if err != nil {
			lastErr = err
		}
	}
	return lastErr
}

// GetStorageNodeIdx returns storage node index for the given at and labels.
//
// The returned index must be passed to WriteDataPoint.
func (ctx *InsertCtx) GetStorageNodeIdx(at *auth.Token, labels []prompb.Label) int {
	if len(storageNodes) == 1 {
		// Fast path - only a single storage node.
		return 0
	}

	buf := ctx.labelsBuf[:0]
	buf = encoding.MarshalUint32(buf, at.AccountID)
	buf = encoding.MarshalUint32(buf, at.ProjectID)
	for i := range labels {
		label := &labels[i]
		buf = marshalBytesFast(buf, label.Name)
		buf = marshalBytesFast(buf, label.Value)
	}
	h := xxhash.Sum64(buf)
	ctx.labelsBuf = buf

	idx := int(jump.Hash(h, int32(len(storageNodes))))
	return idx
}

func marshalBytesFast(dst []byte, s []byte) []byte {
	dst = encoding.MarshalUint16(dst, uint16(len(s)))
	dst = append(dst, s...)
	return dst
}
