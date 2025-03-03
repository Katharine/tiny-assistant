// Copyright 2025 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/Katharine/tiny-assistant/service/assistant/config"
	"github.com/Katharine/tiny-assistant/service/assistant/functions"
	"github.com/Katharine/tiny-assistant/service/assistant/query"
	"github.com/redis/go-redis/v9"
	"google.golang.org/api/iterator"
	"google.golang.org/genai"
	"nhooyr.io/websocket"
)

type PromptSession struct {
	conn             *websocket.Conn
	prompt           string
	query            url.Values
	redis            *redis.Client
	threadId         uuid.UUID
	originalThreadId string
}

type QueryContext struct {
	values url.Values
}

func NewPromptSession(redisClient *redis.Client, rw http.ResponseWriter, r *http.Request) (*PromptSession, error) {
	prompt := r.URL.Query().Get("prompt")
	originalThreadId := r.URL.Query().Get("threadId")
	c, err := websocket.Accept(rw, r, &websocket.AcceptOptions{
		OriginPatterns:     []string{"null"},
		InsecureSkipVerify: true,
	})
	if err != nil {
		return nil, err
	}

	return &PromptSession{
		conn:             c,
		prompt:           prompt,
		query:            r.URL.Query(),
		redis:            redisClient,
		threadId:         uuid.New(),
		originalThreadId: originalThreadId,
	}, nil
}

func (ps *PromptSession) Run(ctx context.Context) {
	ctx = query.ContextWith(ctx, ps.query)
	geminiClient, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  config.GetConfig().GeminiKey,
		Backend: genai.BackendGeminiAPI,
	})
	if err != nil {
		log.Printf("error creating Gemini client: %v\n", err)
		_ = ps.conn.Close(websocket.StatusInternalError, "Error creating client.")
		return
	}

	var messages []*genai.Content
	messages = append(messages, &genai.Content{
		Parts: []*genai.Part{{Text: ps.prompt}},
		Role:  "user",
	})

	if ps.originalThreadId != "" {
		oldMessages, err := ps.restoreThread(ctx, ps.originalThreadId)
		if err != nil {
			log.Printf("error restoring thread: %v\n", err)
			_ = ps.conn.Close(websocket.StatusInternalError, "Error restoring thread.")
			return
		} else {
			messages = append(oldMessages, messages...)
		}
	}
	totalInputTokens := 0
	totalOutputTokens := 0
	iterations := 0
	for {
		cont, err := func() (bool, error) {
			iterations++
			var tools []*genai.Tool
			if iterations <= 10 {
				tools = []*genai.Tool{{FunctionDeclarations: functions.GetFunctionDefinitionsForCapabilities(query.SupportedActionsFromContext(ctx))}}
			}
			streamCtx := ctx

			temperature := float64(0.5)
			one := int64(1)
			s := geminiClient.Models.GenerateContentStream(streamCtx, "models/gemini-2.0-flash", messages, &genai.GenerateContentConfig{
				SystemInstruction: &genai.Content{Parts: []*genai.Part{{Text: ps.generateSystemPrompt(streamCtx)}}},
				Temperature:       &temperature,
				CandidateCount:    &one,
				Tools:             tools,
			})
			var functionCall *genai.FunctionCall
			content := ""
			var usageData *genai.GenerateContentResponseUsageMetadata
			for resp, err := range s {
				if errors.Is(err, iterator.Done) {
					break
				}
				if err != nil {
					log.Printf("recv from Google failed: %v\n", err)
					_ = ps.conn.Close(websocket.StatusInternalError, "request to Google failed")
					return false, err
				}
				usageData = resp.UsageMetadata
				if len(resp.Candidates) == 0 {
					continue
				}
				choice := resp.Candidates[0]
				ourContent := ""
				for _, c := range choice.Content.Parts {
					if c.Text != "" {
						ourContent += c.Text
					}
					if c.FunctionCall != nil {
						fc := *c.FunctionCall
						functionCall = &fc
					}
				}
				if strings.TrimSpace(ourContent) != "" {
					if err := ps.conn.Write(streamCtx, websocket.MessageText, []byte("c"+ourContent)); err != nil {
						log.Printf("write to websocket failed: %v\n", err)
						break
					}
				}
				content += ourContent
			}
			if usageData != nil {
				if usageData.PromptTokenCount != nil {
					totalInputTokens += int(*usageData.PromptTokenCount)
				}
				if usageData.CandidatesTokenCount != nil {
					totalOutputTokens += int(*usageData.CandidatesTokenCount)
				}
			}
			if len(strings.TrimSpace(content)) > 0 {
				messages = append(messages, &genai.Content{
					Parts: []*genai.Part{{Text: content}},
					Role:  "model",
				})
			}
			if functionCall != nil {
				messages = append(messages, &genai.Content{
					Role: "model",
					Parts: []*genai.Part{
						{FunctionCall: functionCall},
					},
				})
				log.Printf("calling function %s\n", functionCall.Name)
				fnBytes, _ := json.Marshal(functionCall.Args)
				fnArgs := string(fnBytes)
				if err := ps.conn.Write(ctx, websocket.MessageText, []byte("f"+functions.SummariseFunction(functionCall.Name, fnArgs))); err != nil {
					log.Printf("write to websocket failed: %v\n", err)
					return false, err
				}
				var result string
				var err error
				if functions.IsAction(functionCall.Name) {
					result, err = functions.CallAction(ctx, functionCall.Name, fnArgs, ps.conn)
				} else {
					result, err = functions.CallFunction(ctx, functionCall.Name, fnArgs)
				}
				if err != nil {
					log.Printf("call function failed: %v\n", err)
					result = "failed to call function: " + err.Error()
				}
				var mapResult map[string]any
				_ = json.Unmarshal([]byte(result), &mapResult)
				messages = append(messages, &genai.Content{
					Role: "function",
					Parts: []*genai.Part{
						{FunctionResponse: &genai.FunctionResponse{
							Name:     functionCall.Name,
							Response: mapResult,
						}},
					},
				})
				return true, nil
			} else {
				if err := ps.conn.Write(ctx, websocket.MessageText, []byte("d")); err != nil {
					log.Printf("write to websocket failed: %v\n", err)
					return false, err
				}
			}
			return false, nil
		}()
		if err != nil {
			return
		}
		if !cont {
			log.Println("Stopping")
			break
		}
		log.Println("Going around again")
	}
	if err := ps.storeThread(ctx, messages); err != nil {
		log.Printf("store thread failed: %v\n", err)
		_ = ps.conn.Close(websocket.StatusInternalError, "store thread failed")
		return
	}
	if err := ps.conn.Write(ctx, websocket.MessageText, []byte("t"+ps.threadId.String())); err != nil {
		log.Printf("store thread ID failed: %s\n", err)
	}
	log.Println("Request handled successfully.")
	_ = ps.conn.Close(websocket.StatusNormalClosure, "")
}

type SerializedMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func (ps *PromptSession) storeThread(ctx context.Context, messages []*genai.Content) error {
	var toStore []SerializedMessage
	for _, m := range messages {
		if len(m.Parts) != 0 && (m.Role == "user" || m.Role == "model") && len(strings.TrimSpace(m.Parts[0].Text)) > 0 {
			toStore = append(toStore, SerializedMessage{
				Content: m.Parts[0].Text,
				Role:    m.Role,
			})
		}
	}
	j, err := json.Marshal(toStore)
	if err != nil {
		return err
	}
	ps.redis.Set(ctx, "thread:"+ps.threadId.String(), j, 10*time.Minute)
	return nil
}

func (ps *PromptSession) restoreThread(ctx context.Context, oldThreadId string) ([]*genai.Content, error) {
	j, err := ps.redis.Get(ctx, "thread:"+oldThreadId).Result()
	if err != nil {
		return nil, err
	}
	var messages []SerializedMessage
	if err := json.Unmarshal([]byte(j), &messages); err != nil {
		return nil, err
	}
	var result []*genai.Content
	for _, m := range messages {
		result = append(result, &genai.Content{
			Parts: []*genai.Part{{Text: m.Content}},
			Role:  m.Role,
		})
	}
	return result, nil
}
