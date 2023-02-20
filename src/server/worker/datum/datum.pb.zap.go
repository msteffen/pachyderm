// Code generated by protoc-gen-zap (etc/proto/protoc-gen-zap). DO NOT EDIT.
//
// source: server/worker/datum/datum.proto

package datum

import (
	zapcore "go.uber.org/zap/zapcore"
)

func (x *Meta) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	if x == nil {
		return nil
	}
	enc.AddObject("job", x.Job)
	inputsArrMarshaller := func(enc zapcore.ArrayEncoder) error {
		for _, v := range x.Inputs {
			if obj, ok := interface{}(v).(zapcore.ObjectMarshaler); ok {
				enc.AppendObject(obj)
			} else {
				enc.AppendReflected(v)
			}
		}
		return nil
	}
	enc.AddArray("inputs", zapcore.ArrayMarshalerFunc(inputsArrMarshaller))
	enc.AddString("hash", x.Hash)
	enc.AddString("state", x.State.String())
	enc.AddString("reason", x.Reason)
	enc.AddObject("stats", x.Stats)
	enc.AddInt64("index", x.Index)
	enc.AddString("image_id", x.ImageId)
	return nil
}

func (x *Stats) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	if x == nil {
		return nil
	}
	enc.AddObject("process_stats", x.ProcessStats)
	enc.AddInt64("processed", x.Processed)
	enc.AddInt64("skipped", x.Skipped)
	enc.AddInt64("total", x.Total)
	enc.AddInt64("failed", x.Failed)
	enc.AddInt64("recovered", x.Recovered)
	enc.AddString("failed_id", x.FailedID)
	return nil
}

func (x *PFSTask) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	if x == nil {
		return nil
	}
	enc.AddObject("input", x.Input)
	enc.AddObject("path_range", x.PathRange)
	enc.AddInt64("base_index", x.BaseIndex)
	enc.AddString("auth_token", x.AuthToken)
	return nil
}

func (x *PFSTaskResult) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	if x == nil {
		return nil
	}
	enc.AddString("file_set_id", x.FileSetId)
	return nil
}

func (x *CrossTask) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	if x == nil {
		return nil
	}
	file_set_idsArrMarshaller := func(enc zapcore.ArrayEncoder) error {
		for _, v := range x.FileSetIds {
			enc.AppendString(v)
		}
		return nil
	}
	enc.AddArray("file_set_ids", zapcore.ArrayMarshalerFunc(file_set_idsArrMarshaller))
	enc.AddInt64("base_file_set_index", x.BaseFileSetIndex)
	enc.AddObject("base_file_set_path_range", x.BaseFileSetPathRange)
	enc.AddInt64("base_index", x.BaseIndex)
	enc.AddString("auth_token", x.AuthToken)
	return nil
}

func (x *CrossTaskResult) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	if x == nil {
		return nil
	}
	enc.AddString("file_set_id", x.FileSetId)
	return nil
}

func (x *KeyTask) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	if x == nil {
		return nil
	}
	enc.AddString("file_set_id", x.FileSetId)
	enc.AddObject("path_range", x.PathRange)
	enc.AddString("type", x.Type.String())
	enc.AddString("auth_token", x.AuthToken)
	return nil
}

func (x *KeyTaskResult) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	if x == nil {
		return nil
	}
	enc.AddString("file_set_id", x.FileSetId)
	return nil
}

func (x *MergeTask) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	if x == nil {
		return nil
	}
	file_set_idsArrMarshaller := func(enc zapcore.ArrayEncoder) error {
		for _, v := range x.FileSetIds {
			enc.AppendString(v)
		}
		return nil
	}
	enc.AddArray("file_set_ids", zapcore.ArrayMarshalerFunc(file_set_idsArrMarshaller))
	enc.AddObject("path_range", x.PathRange)
	enc.AddString("type", x.Type.String())
	enc.AddString("auth_token", x.AuthToken)
	return nil
}

func (x *MergeTaskResult) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	if x == nil {
		return nil
	}
	enc.AddString("file_set_id", x.FileSetId)
	return nil
}

func (x *ComposeTask) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	if x == nil {
		return nil
	}
	file_set_idsArrMarshaller := func(enc zapcore.ArrayEncoder) error {
		for _, v := range x.FileSetIds {
			enc.AppendString(v)
		}
		return nil
	}
	enc.AddArray("file_set_ids", zapcore.ArrayMarshalerFunc(file_set_idsArrMarshaller))
	enc.AddString("auth_token", x.AuthToken)
	return nil
}

func (x *ComposeTaskResult) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	if x == nil {
		return nil
	}
	enc.AddString("file_set_id", x.FileSetId)
	return nil
}

func (x *SetSpec) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	if x == nil {
		return nil
	}
	enc.AddInt64("number", x.Number)
	enc.AddInt64("size_bytes", x.SizeBytes)
	return nil
}