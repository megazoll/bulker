package file_storage

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/jitsucom/bulker/base/errorj"
	"github.com/jitsucom/bulker/base/logging"
	"github.com/jitsucom/bulker/base/utils"
	"github.com/jitsucom/bulker/bulker"
	"github.com/jitsucom/bulker/implementations"
	"github.com/jitsucom/bulker/types"
	jsoniter "github.com/json-iterator/go"
	"os"
	"path"
	"sort"
	"strings"
	"time"
)

type AbstractFileStorageStream struct {
	id           string
	mode         bulker.BulkMode
	fileAdapter  implementations.FileAdapter
	options      bulker.StreamOptions
	filenameFunc func() string

	flatten         bool
	merge           bool
	pkColumns       utils.Set[string]
	timestampColumn string

	batchFile          *os.File
	marshaller         types.Marshaller
	targetMarshaller   types.Marshaller
	eventsInBatch      int
	batchFileLinesByPK map[string]int
	batchFileSkipLines utils.Set[int]
	csvHeader          utils.Set[string]

	firstEventTime time.Time
	lastEventTime  time.Time

	state  bulker.State
	inited bool
}

func newAbstractFileStorageStream(id string, p implementations.FileAdapter, filenameFunc func() string, mode bulker.BulkMode, streamOptions ...bulker.StreamOption) (AbstractFileStorageStream, error) {
	ps := AbstractFileStorageStream{id: id, fileAdapter: p, filenameFunc: filenameFunc, mode: mode}
	ps.options = bulker.StreamOptions{}
	for _, option := range streamOptions {
		ps.options.Add(option)
	}
	ps.merge = bulker.MergeRowsOption.Get(&ps.options)
	pkColumns := bulker.PrimaryKeyOption.Get(&ps.options)
	if ps.merge && len(pkColumns) == 0 {
		return AbstractFileStorageStream{}, fmt.Errorf("MergeRows option requires primary key option. Please provide WithPrimaryKey option")
	}
	ps.pkColumns = pkColumns
	ps.timestampColumn = bulker.TimestampOption.Get(&ps.options)
	if ps.merge {
		ps.batchFileLinesByPK = make(map[string]int)
		ps.batchFileSkipLines = utils.NewSet[int]()
	}
	ps.state = bulker.State{Status: bulker.Active}
	return ps, nil
}

func (ps *AbstractFileStorageStream) init(ctx context.Context) error {
	if ps.inited {
		return nil
	}

	if ps.batchFile == nil {
		var err error
		ps.batchFile, err = os.CreateTemp("", fmt.Sprintf("bulker_%s", utils.SanitizeString(ps.id)))
		if err != nil {
			return err
		}
		ps.marshaller = &types.JSONMarshaller{}
		ps.targetMarshaller, err = types.NewMarshaller(ps.fileAdapter.Format(), ps.fileAdapter.Compression())
		if err != nil {
			return err
		}
		if ps.fileAdapter.Format() == types.FileFormatCSV || ps.fileAdapter.Format() == types.FileFormatNDJSONFLAT {
			ps.flatten = true
		}
	}
	ps.inited = true
	return nil
}

func (ps *AbstractFileStorageStream) preprocess(object types.Object) (types.Object, error) {
	if ps.flatten {
		flatObject, err := implementations.DefaultFlattener.FlattenObject(object, nil)
		if err != nil {
			return nil, err
		} else {
			return flatObject, nil
		}
	}
	return object, nil
}

func (ps *AbstractFileStorageStream) postConsume(err error) error {
	if err != nil {
		ps.state.ErrorRowIndex = ps.state.ProcessedRows
		ps.state.SetError(err)
		return err
	} else {
		ps.state.SuccessfulRows++
	}
	return nil
}

func (ps *AbstractFileStorageStream) postComplete(err error) (bulker.State, error) {
	_ = ps.batchFile.Close()
	_ = os.Remove(ps.batchFile.Name())
	if err != nil {
		ps.state.SetError(err)
		ps.state.Status = bulker.Failed
	} else {
		ps.state.Status = bulker.Completed
	}
	return ps.state, err
}

func (ps *AbstractFileStorageStream) flushBatchFile(ctx context.Context) (err error) {
	defer func() {
		if ps.merge {
			ps.batchFileLinesByPK = make(map[string]int)
			ps.batchFileSkipLines = utils.NewSet[int]()
		}
		_ = ps.batchFile.Close()
		_ = os.Remove(ps.batchFile.Name())
	}()
	if ps.eventsInBatch > 0 {

		err = ps.marshaller.Flush()
		if err != nil {
			return errorj.Decorate(err, "failed to flush marshaller")
		}
		err = ps.batchFile.Sync()
		if err != nil {
			return errorj.Decorate(err, "failed to sync batch file")
		}
		workingFile := ps.batchFile
		needToConvert := false
		convertStart := time.Now()
		if !ps.targetMarshaller.Equal(ps.marshaller) {
			needToConvert = true
		}
		if len(ps.batchFileSkipLines) > 0 || needToConvert {
			workingFile, err = os.CreateTemp("", path.Base(ps.batchFile.Name())+"_2")
			if err != nil {
				return errorj.Decorate(err, "failed to create tmp file for deduplication")
			}
			defer func() {
				_ = workingFile.Close()
				_ = os.Remove(workingFile.Name())
			}()
			if needToConvert {
				header := ps.csvHeader.ToSlice()
				sort.Strings(header)
				err = ps.targetMarshaller.Init(workingFile, header)
				if err != nil {
					return errorj.Decorate(err, "failed to write header for converted batch file")
				}
			}
			file, err := os.Open(ps.batchFile.Name())
			if err != nil {
				return errorj.Decorate(err, "failed to open tmp file")
			}
			scanner := bufio.NewScanner(file)
			i := 0
			for scanner.Scan() {
				if !ps.batchFileSkipLines.Contains(i) {
					if needToConvert {
						dec := jsoniter.NewDecoder(bytes.NewReader(scanner.Bytes()))
						dec.UseNumber()
						obj := make(map[string]any)
						err = dec.Decode(&obj)
						if err != nil {
							return errorj.Decorate(err, "failed to decode json object from batch filer")
						}
						ps.targetMarshaller.Marshal(obj)
					} else {
						_, err = workingFile.Write(scanner.Bytes())
						if err != nil {
							return errorj.Decorate(err, "failed write to deduplication file")
						}
						_, _ = workingFile.Write([]byte("\n"))
					}
				}
				i++
			}
			ps.targetMarshaller.Flush()
			workingFile.Sync()
		}
		if needToConvert {
			logging.Infof("[%s] Converted batch file from %s to %s in %s", ps.id, ps.marshaller.Format(), ps.targetMarshaller.Format(), time.Now().Sub(convertStart))
		}
		//create file reader for workingFile
		_, err = workingFile.Seek(0, 0)
		if err != nil {
			return errorj.Decorate(err, "failed to seek to beginning of tmp file")
		}
		err = ps.fileAdapter.Upload(ps.filenameFunc(), workingFile)
		if err != nil {
			return errorj.Decorate(err, "failed to flush tmp file to the warehouse")
		}
	}
	return nil
}

func (ps *AbstractFileStorageStream) getPKValue(object types.Object) (string, error) {
	l := len(ps.pkColumns)
	if l == 0 {
		return "", fmt.Errorf("primary key is not set")
	}
	if l == 1 {
		for col := range ps.pkColumns {
			pkValue, ok := object[col]
			if !ok {
				return "", fmt.Errorf("primary key [%s] is not found in the object", col)
			}
			return fmt.Sprint(pkValue), nil
		}
	}
	var builder strings.Builder
	for col := range ps.pkColumns {
		pkValue, ok := object[col]
		if ok {
			builder.WriteString(fmt.Sprint(pkValue))
			builder.WriteString("_")
		}
	}
	if builder.Len() > 0 {
		return builder.String(), nil
	}
	return "", fmt.Errorf("primary key columns not found in the object")
}

func (ps *AbstractFileStorageStream) writeToBatchFile(ctx context.Context, processedObject types.Object) error {
	header := ps.csvHeader.ToSlice()
	sort.Strings(header)
	ps.marshaller.Init(ps.batchFile, header)
	if ps.merge {
		pk, err := ps.getPKValue(processedObject)
		if err != nil {
			return err
		}
		line, ok := ps.batchFileLinesByPK[pk]
		if ok {
			ps.batchFileSkipLines.Put(line)
		}
		lineNumber := ps.eventsInBatch
		if ps.marshaller.NeedHeader() {
			lineNumber++
		}
		ps.batchFileLinesByPK[pk] = lineNumber
	}
	err := ps.marshaller.Marshal(processedObject)
	if err != nil {
		return errorj.Decorate(err, "failed to marshall into csv file")
	}
	ps.eventsInBatch++
	return nil
}

func (ps *AbstractFileStorageStream) Consume(ctx context.Context, object types.Object) (state bulker.State, processedObjects []types.Object, err error) {
	defer func() {
		err = ps.postConsume(err)
		state = ps.state
	}()
	if err = ps.init(ctx); err != nil {
		return
	}
	eventTime := ps.getEventTime(object)
	if ps.lastEventTime.IsZero() || eventTime.After(ps.lastEventTime) {
		ps.lastEventTime = eventTime
	}
	if ps.firstEventTime.IsZero() || eventTime.Before(ps.firstEventTime) {
		ps.firstEventTime = eventTime
	}

	//type mapping, flattening => table schema
	processedObject, err := ps.preprocess(object)
	if err != nil {
		return
	}

	if ps.targetMarshaller.Format() == "csv" {
		ps.csvHeader.PutAllKeys(processedObject)
	}

	err = ps.writeToBatchFile(ctx, processedObject)

	return
}

func (ps *AbstractFileStorageStream) Abort(ctx context.Context) (state bulker.State, err error) {
	if ps.state.Status != bulker.Active {
		return ps.state, errors.New("stream is not active")
	}
	if ps.batchFile != nil {
		_ = ps.batchFile.Close()
		_ = os.Remove(ps.batchFile.Name())
	}
	ps.state.Status = bulker.Aborted
	return ps.state, err
}

func (ps *AbstractFileStorageStream) getEventTime(object types.Object) time.Time {
	if ps.timestampColumn != "" {
		tm, ok := types.ReformatTimeValue(object[ps.timestampColumn]).(time.Time)
		if ok {
			return tm
		}
	}
	return time.Now()
}

func (ps *AbstractFileStorageStream) Complete(ctx context.Context) (state bulker.State, err error) {
	if ps.state.Status != bulker.Active {
		return ps.state, errors.New("stream is not active")
	}
	defer func() {
		state, err = ps.postComplete(err)
	}()
	if ps.state.LastError == nil {
		//if at least one object was inserted
		if ps.state.SuccessfulRows > 0 {
			if ps.batchFile != nil {
				if err = ps.flushBatchFile(ctx); err != nil {
					return ps.state, err
				}
			}
		}
		return
	} else {
		//if was any error - it will trigger transaction rollback in defer func
		err = ps.state.LastError
		return
	}
}
