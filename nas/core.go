package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"duanjuapp/native/core"
)

type coreError struct {
	message string
	code    string
	retryAt time.Time
}

func (err *coreError) Error() string { return err.message }

var coreSlots = make(chan struct{}, 3)

func coreCall(action string, fields map[string]any) (json.RawMessage, error) {
	if action == "" {
		return nil, errors.New("缺少操作")
	}
	payload := make(map[string]any, len(fields)+1)
	payload["action"] = action
	for key, value := range fields {
		if value != nil {
			payload[key] = value
		}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	coreSlots <- struct{}{}
	defer func() { <-coreSlots }()
	var envelope struct {
		OK      bool            `json:"ok"`
		Data    json.RawMessage `json:"data"`
		Error   string          `json:"error"`
		Code    string          `json:"code"`
		RetryAt time.Time       `json:"retryAt"`
	}
	if decodeErr := json.Unmarshal([]byte(core.NativeRequest(string(body))), &envelope); decodeErr != nil {
		return nil, errors.New("本地核心返回无法解析")
	}
	if !envelope.OK {
		message := envelope.Error
		if message == "" {
			message = "操作失败"
		}
		return nil, &coreError{message: message, code: envelope.Code, retryAt: envelope.RetryAt}
	}
	return envelope.Data, nil
}

func coreInitialize(directory string) (map[string]any, error) {
	data, err := coreCall("initialize", map[string]any{"directory": directory})
	if err != nil {
		return nil, err
	}
	var info map[string]any
	if json.Unmarshal(data, &info) != nil {
		return nil, fmt.Errorf("核心版本信息无效")
	}
	return info, nil
}
