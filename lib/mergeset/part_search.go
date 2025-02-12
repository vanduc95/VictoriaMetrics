package mergeset

import (
	"fmt"
	"io"
	"sort"

	"github.com/VictoriaMetrics/VictoriaMetrics/lib/blockcache"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/bytesutil"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/encoding"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/logger"
)

type partSearch struct {
	// Item contains the last item found after the call to NextItem.
	//
	// The Item content is valid until the next call to NextItem.
	Item []byte

	// p is a part to search.
	p *part

	// The remaining metaindex rows to scan, obtained from p.mrs.
	mrs []metaindexRow

	// The remaining block headers to scan in the current metaindexRow.
	bhs []blockHeader

	// err contains the last error.
	err error

	indexBuf           []byte
	compressedIndexBuf []byte

	sb storageBlock

	ib        *inmemoryBlock
	ibItemIdx int
}

func (ps *partSearch) reset() {
	ps.Item = nil
	ps.p = nil
	ps.mrs = nil
	ps.bhs = nil
	ps.err = nil

	ps.indexBuf = ps.indexBuf[:0]
	ps.compressedIndexBuf = ps.compressedIndexBuf[:0]

	ps.sb.Reset()

	ps.ib = nil
	ps.ibItemIdx = 0
}

// Init initializes ps for search in the p.
//
// Use Seek for search in p.
func (ps *partSearch) Init(p *part) {
	ps.reset()

	ps.p = p
}

// Seek seeks for the first item greater or equal to k in ps.
func (ps *partSearch) Seek(k []byte) {
	if err := ps.Error(); err != nil {
		// Do nothing on unrecoverable error.
		return
	}
	ps.err = nil

	if string(k) > string(ps.p.ph.lastItem) {
		// Not matching items in the part.
		ps.err = io.EOF
		return
	}

	if ps.tryFastSeek(k) {
		return
	}

	ps.Item = nil
	ps.mrs = ps.p.mrs
	ps.bhs = nil

	ps.indexBuf = ps.indexBuf[:0]
	ps.compressedIndexBuf = ps.compressedIndexBuf[:0]

	ps.sb.Reset()

	ps.ib = nil
	ps.ibItemIdx = 0

	if string(k) <= string(ps.p.ph.firstItem) {
		// The first item in the first block matches.
		ps.err = ps.nextBlock()
		return
	}

	// Locate the first metaindexRow to scan.
	if len(ps.mrs) == 0 {
		logger.Panicf("BUG: part without metaindex rows passed to partSearch")
	}
	n := sort.Search(len(ps.mrs), func(i int) bool {
		return string(k) <= string(ps.mrs[i].firstItem)
	})
	if n > 0 {
		// The given k may be located in the previous metaindexRow, so go to it.
		n--
	}
	ps.mrs = ps.mrs[n:]

	// Read block headers for the found metaindexRow.
	if err := ps.nextBHS(); err != nil {
		ps.err = err
		return
	}

	// Locate the first block to scan.
	n = sort.Search(len(ps.bhs), func(i int) bool {
		return string(k) <= string(ps.bhs[i].firstItem)
	})
	if n > 0 {
		// The given k may be located in the previous block, so go to it.
		n--
	}
	ps.bhs = ps.bhs[n:]

	// Read the block.
	if err := ps.nextBlock(); err != nil {
		ps.err = err
		return
	}

	// Locate the first item to scan in the block.
	items := ps.ib.items
	data := ps.ib.data
	cpLen := commonPrefixLen(ps.ib.commonPrefix, k)
	if cpLen > 0 {
		keySuffix := k[cpLen:]
		ps.ibItemIdx = sort.Search(len(items), func(i int) bool {
			it := items[i]
			it.Start += uint32(cpLen)
			return string(keySuffix) <= it.String(data)
		})
	} else {
		ps.ibItemIdx = binarySearchKey(data, items, k)
	}
	if ps.ibItemIdx < len(items) {
		// The item has been found.
		return
	}

	// Nothing found in the current block. Proceed to the next block.
	// The item to search must be the first in the next block.
	if err := ps.nextBlock(); err != nil {
		ps.err = err
		return
	}
}

func (ps *partSearch) tryFastSeek(k []byte) bool {
	if ps.ib == nil {
		return false
	}
	data := ps.ib.data
	items := ps.ib.items
	idx := ps.ibItemIdx
	if idx >= len(items) {
		// The ib is exhausted.
		return false
	}
	if string(k) > items[len(items)-1].String(data) {
		// The item is located in next blocks.
		return false
	}

	// The item is located either in the current block or in previous blocks.
	if idx > 0 {
		idx--
	}
	if string(k) < items[idx].String(data) {
		if string(k) < items[0].String(data) {
			// The item is located in previous blocks.
			return false
		}
		idx = 0
	}

	// The item is located in the current block
	ps.ibItemIdx = idx + binarySearchKey(data, items[idx:], k)
	return true
}

// NextItem advances to the next Item.
//
// Returns true on success.
func (ps *partSearch) NextItem() bool {
	if ps.err != nil {
		return false
	}

	items := ps.ib.items
	if ps.ibItemIdx < len(items) {
		// Fast path - the current block contains more items.
		// Proceed to the next item.
		ps.Item = items[ps.ibItemIdx].Bytes(ps.ib.data)
		ps.ibItemIdx++
		return true
	}

	// The current block is over. Proceed to the next block.
	if err := ps.nextBlock(); err != nil {
		if err != io.EOF {
			err = fmt.Errorf("error in %q: %w", ps.p.path, err)
		}
		ps.err = err
		return false
	}

	// Invariant: len(ps.ib.items) > 0 after nextBlock.
	ps.Item = ps.ib.items[0].Bytes(ps.ib.data)
	ps.ibItemIdx++
	return true
}

// Error returns the last error occurred in the ps.
func (ps *partSearch) Error() error {
	if ps.err == io.EOF {
		return nil
	}
	return ps.err
}

func (ps *partSearch) nextBlock() error {
	if len(ps.bhs) == 0 {
		// The current metaindexRow is over. Proceed to the next metaindexRow.
		if err := ps.nextBHS(); err != nil {
			return err
		}
	}
	bh := &ps.bhs[0]
	ps.bhs = ps.bhs[1:]
	ib, err := ps.getInmemoryBlock(bh)
	if err != nil {
		return err
	}
	ps.ib = ib
	ps.ibItemIdx = 0
	return nil
}

func (ps *partSearch) nextBHS() error {
	if len(ps.mrs) == 0 {
		return io.EOF
	}
	mr := &ps.mrs[0]
	ps.mrs = ps.mrs[1:]
	idxbKey := blockcache.Key{
		Part:   ps.p,
		Offset: mr.indexBlockOffset,
	}
	b := idxbCache.GetBlock(idxbKey)
	if b == nil {
		idxb, err := ps.readIndexBlock(mr)
		if err != nil {
			return fmt.Errorf("cannot read index block: %w", err)
		}
		b = idxb
		idxbCache.PutBlock(idxbKey, b)
	}
	idxb := b.(*indexBlock)
	ps.bhs = idxb.bhs
	return nil
}

func (ps *partSearch) readIndexBlock(mr *metaindexRow) (*indexBlock, error) {
	ps.compressedIndexBuf = bytesutil.ResizeNoCopyMayOverallocate(ps.compressedIndexBuf, int(mr.indexBlockSize))
	ps.p.indexFile.MustReadAt(ps.compressedIndexBuf, int64(mr.indexBlockOffset))

	var err error
	ps.indexBuf, err = encoding.DecompressZSTD(ps.indexBuf[:0], ps.compressedIndexBuf)
	if err != nil {
		return nil, fmt.Errorf("cannot decompress index block: %w", err)
	}
	idxb := &indexBlock{}
	idxb.bhs, err = unmarshalBlockHeaders(idxb.bhs[:0], ps.indexBuf, int(mr.blockHeadersCount))
	if err != nil {
		return nil, fmt.Errorf("cannot unmarshal block headers from index block (offset=%d, size=%d): %w", mr.indexBlockOffset, mr.indexBlockSize, err)
	}
	return idxb, nil
}

func (ps *partSearch) getInmemoryBlock(bh *blockHeader) (*inmemoryBlock, error) {
	ibKey := blockcache.Key{
		Part:   ps.p,
		Offset: bh.itemsBlockOffset,
	}
	b := ibCache.GetBlock(ibKey)
	if b == nil {
		ib, err := ps.readInmemoryBlock(bh)
		if err != nil {
			return nil, err
		}
		b = ib
		ibCache.PutBlock(ibKey, b)
	}
	ib := b.(*inmemoryBlock)
	return ib, nil
}

func (ps *partSearch) readInmemoryBlock(bh *blockHeader) (*inmemoryBlock, error) {
	ps.sb.Reset()

	ps.sb.itemsData = bytesutil.ResizeNoCopyMayOverallocate(ps.sb.itemsData, int(bh.itemsBlockSize))
	ps.p.itemsFile.MustReadAt(ps.sb.itemsData, int64(bh.itemsBlockOffset))

	ps.sb.lensData = bytesutil.ResizeNoCopyMayOverallocate(ps.sb.lensData, int(bh.lensBlockSize))
	ps.p.lensFile.MustReadAt(ps.sb.lensData, int64(bh.lensBlockOffset))

	ib := getInmemoryBlock()
	if err := ib.UnmarshalData(&ps.sb, bh.firstItem, bh.commonPrefix, bh.itemsCount, bh.marshalType); err != nil {
		return nil, fmt.Errorf("cannot unmarshal storage block with %d items: %w", bh.itemsCount, err)
	}

	return ib, nil
}

func binarySearchKey(data []byte, items []Item, key []byte) int {
	if len(items) == 0 {
		return 0
	}
	if string(key) <= items[0].String(data) {
		// Fast path - the item is the first.
		return 0
	}
	items = items[1:]
	offset := uint(1)

	// This has been copy-pasted from https://golang.org/src/sort/search.go
	n := uint(len(items))
	i, j := uint(0), n
	for i < j {
		h := uint(i+j) >> 1
		if h >= 0 && h < uint(len(items)) && string(key) > items[h].String(data) {
			i = h + 1
		} else {
			j = h
		}
	}
	return int(i + offset)
}
