package pos

import (
	"fmt"
	"io"
	"math"
	"time"

	"github.com/spf13/afero"

	"github.com/kargakis/gochia/pkg/parameters"
	"github.com/kargakis/gochia/pkg/serialize"
	"github.com/kargakis/gochia/pkg/utils"
	"github.com/kargakis/gochia/pkg/utils/sort"
)

var AppFs = afero.NewOsFs()

// This is Phase 1, or forward propagation. During this phase, all of the 7 tables,
// and f functions, are evaluated. The result is an intermediate plot file, that is
// several times larger than what the final file will be, but that has all of the
// proofs of space in it. First, F1 is computed, which is special since it uses
// AES256, and each encryption provides multiple output values. Then, the rest of the
// f functions are computed, and a sort on disk happens for each table.
func WritePlotFile(filename string, k, availableMemory int, memo, id []byte) error {
	file, err := AppFs.Create(filename)
	if err != nil {
		return err
	}

	headerLen, err := WriteHeader(file, k, memo, id)
	if err != nil {
		return err
	}

	fmt.Println("Computing table 1...")
	start := time.Now()
	wrote, err := WriteFirstTable(file, k, headerLen, id)
	if err != nil {
		return err
	}

	// if we know beforehand there is not enough space
	// to sort in memory, we can prepare the spare file
	var spare afero.File
	if wrote > availableMemory {
		spare, err = AppFs.Create(filename + "-spare")
		if err != nil {
			return err
		}
	}

	fmt.Println("Sorting table 1...")
	maxNumber := int(math.Pow(2, float64(k)))
	entryLen := wrote / maxNumber
	if err := sort.OnDisk(file, spare, headerLen, wrote+headerLen, availableMemory, entryLen, maxNumber, k); err != nil {
		return err
	}
	fmt.Printf("F1 calculations finished in %v (wrote %s)\n", time.Since(start), utils.PrettySize(wrote))

	fmt.Println("Computing table 2...")
	start = time.Now()
	fx, err := NewFx(uint64(k), id)
	if err != nil {
		return err
	}

	previousStart := headerLen
	currentStart := headerLen + wrote
	for t := 2; t <= 7; t++ {
		wrote, err := WriteTable(file, k, t, previousStart, currentStart, entryLen, fx)
		if err != nil {
			return err
		}
		previousStart += wrote
		currentStart += wrote
		entryLen = wrote / maxNumber
		break // TODO: REMOVE
	}

	return nil
}

func WriteFirstTable(file afero.File, k, start int, id []byte) (int, error) {
	f1, err := NewF1(k, id)
	if err != nil {
		return 0, err
	}

	var wrote int
	maxNumber := uint64(math.Pow(2, float64(k)))

	// TODO: Batch writes
	for x := uint64(0); x < maxNumber; x++ {
		f1x := f1.Calculate(x)
		n, err := serialize.Write(file, int64(start+wrote), x, f1x, k)
		if err != nil {
			return wrote + n, err
		}
		wrote += n
	}
	if _, err := file.Write([]byte(serialize.EOT)); err != nil {
		return wrote, err
	}
	fmt.Printf("Wrote %d entries (size: %s)\n", maxNumber, utils.PrettySize(wrote))
	return wrote, nil
}

// WriteTable reads the t-1'th table from the file and writes the t'th table.
func WriteTable(file afero.File, k, t, previousStart, currentStart, entryLen int, fx *Fx) (int, error) {
	var (
		read    int
		written int

		bucketID     uint64
		leftBucketID uint64
		leftBucket   []*serialize.Entry
		rightBucket  []*serialize.Entry
	)

	var index int

	for {
		// Read an entry
		leftEntry, bytesRead, err := serialize.Read(file, int64(previousStart+read), entryLen, k)
		if err == serialize.EOTErr || err == io.EOF {
			break
		}
		if err != nil {
			return written, fmt.Errorf("cannot read left entry: %v", err)
		}
		read += bytesRead
		leftEntry.Index = index

		leftBucketID = parameters.BucketID(leftEntry.Fx)
		switch {
		case leftBucketID == bucketID:
			// Add entries in the left bucket
			leftBucket = append(leftBucket, leftEntry)

		case leftBucketID == bucketID+1:
			// Add entries in the right bucket
			rightBucket = append(rightBucket, leftEntry)

		default:
			if len(leftBucket) > 0 && len(rightBucket) > 0 {
				// We have finished adding to both buckets, now we need to compare them.
				// For any matches, we are going to calculate outputs for the next table.
				for _, m := range FindMatches(leftBucket, rightBucket) {
					f, err := fx.Calculate(t, m.Left, m.LeftMetadata, m.RightMetadata)
					if err != nil {
						return written, err
					}
					// This is the collated output stored next to the entry - useful
					// for generating outputs for the next table.
					collated, err := Collate(t, uint64(k), m.LeftMetadata, m.RightMetadata)
					if err != nil {
						return written, err
					}
					// Now write the new output in the next table.
					w, err := serialize.Write(file, int64(currentStart+written), f, nil, nil, nil, collated, k)
					if err != nil {
						return written + w, err
					}
					written += w
				}
			}
			if leftBucketID == bucketID+2 {
				// Keep the right bucket as the new left bucket
				bucketID++
				leftBucket = rightBucket
				rightBucket = nil
			} else {
				// This bucket id is greater than bucketID+2 so we need to
				// start over building both buckets.
				bucketID = leftBucketID
				leftBucket = nil
				rightBucket = nil
			}
		}

		// advance the table index
		index++
	}

	return written, nil
}

// WriteHeader writes the plot file header to a file
// 19 bytes  - "Proof of Space Plot" (utf-8)
// 32 bytes  - unique plot id
// 1 byte    - k
// 2 bytes   - memo length
// x bytes   - memo
func WriteHeader(file afero.File, k int, memo, id []byte) (int, error) {
	n, err := file.Write([]byte("Proof of Space Plot"))
	if err != nil {
		return n, err
	}

	nmore, err := file.Write(id)
	n += nmore
	if err != nil {
		return n, err
	}

	nmore, err = file.Write([]byte{byte(k)})
	n += nmore
	if err != nil {
		return n, err
	}

	sizeBuf := make([]byte, 2)
	sizeBuf[0] = byte(len(memo))
	nmore, err = file.Write(sizeBuf)
	n += nmore
	if err != nil {
		return n, err
	}

	nmore, err = file.Write(memo)
	return n + nmore, err
}
