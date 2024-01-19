package main

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"strings"
	"io"
	"encoding/json"
)

type Usage struct {
	PromptTokens int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens int `json:"total_tokens"`
}

type Message struct {
	Role string `json:"role"`
	Content string `json:"content"`
}

type Delta struct {
	Role string `json:"role"`
	Content string `json:"content"`
}

type Choice struct {
	Index int `json:"index"`
	FinishReason string `json:"finish_reason"`
	Message Message `json:"message"`
	Delta Delta `json:"delta"`
}

type PerprexityRes struct {
	Id string `json:"id"`
	Model string `json:"model"`
	Created int `json:"created"`
	Usage Usage `json:"usage"`
	Object string `json:"object"`
	Choices []Choice `json:"choices"`
}

type PerprexityReq struct {
	Model string `json:"model"`
	Messages []Message `json:"messages"`
}

func GeneratePerplexityAIResponse(cfg *Config, r *http.Request) (string, error) {
	// Create a prompt based on the HTTP request
	httpReq, err := httputil.DumpRequest(r, true)
	if err != nil {
		log.Fatal(err)
	}
	prompt := fmt.Sprintf(cfg.PromptTemplate, httpReq)

	// Set up API call options
	url := "https://api.perplexity.ai/chat/completions"
	var perprexityRes PerprexityRes
  perprexityReq := PerprexityReq {
  	Model: cfg.Model,
  	Messages: []Message{{
  		Role: "system",
  		Content: "Be precise and concise.",
		},{
			Role: "user",
  		Content: string(prompt),
		}},
  }
  payload, err := json.Marshal(perprexityReq)
  if err != nil {
    log.Printf("Error marshalling JSON request: %v", err)
		return "", err
  }

	req, _ := http.NewRequest("POST", url, bytes.NewBuffer(payload))

	req.Header.Add("accept", "application/json")
	req.Header.Add("content-type", "application/json")
	req.Header.Add("authorization", "Bearer " + cfg.APIKey)

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("Error generating Perplexity AI response: %v", err)
		return "", err
	}

	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	err = json.Unmarshal(body, &perprexityRes)
	if err != nil {
    log.Printf("Error unmarshalling JSON response: %v", err)
		return "", err
  }
	if len(perprexityRes.Choices) > 0 {
		return strings.TrimSpace(perprexityRes.Choices[0].Message.Content), nil
	}

	return "", errors.New("no valid response from Perplexity AI")
}
