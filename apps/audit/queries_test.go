package audit

import (
	"encoding/json"
	"testing"
)

// Both cases are real and were verified against the live services on
// 2026-08-03. Pollinations is the one that breaks the naive answer: POST
// /openai/chat/completions returns 200 while POST /chat/completions returns 404,
// so publishing the bare host as OPENAI_BASE_URL hands out a snippet that
// cannot work.
func TestOpenAIBaseURLIsWhatAClientCanActuallyCall(t *testing.T) {
	cases := []struct {
		name  string
		model WorkingModel
		want  string
	}{
		{
			name: "path already ends in chat/completions",
			model: WorkingModel{
				BaseURL:  "https://oai.endpoints.kepler.ai.cloud.ovh.net",
				ChatPath: "/v1/chat/completions",
			},
			want: "https://oai.endpoints.kepler.ai.cloud.ovh.net/v1",
		},
		{
			name: "provider mounts the openai surface on its own prefix",
			model: WorkingModel{
				BaseURL:  "https://text.pollinations.ai",
				ChatPath: "/openai",
			},
			want: "https://text.pollinations.ai/openai",
		},
		{
			name: "trailing slash on the base is not doubled",
			model: WorkingModel{
				BaseURL:  "https://api.llm7.io/",
				ChatPath: "/v1/chat/completions",
			},
			want: "https://api.llm7.io/v1",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := testCase.model.OpenAIBaseURL(); got != testCase.want {
				t.Fatalf("OpenAIBaseURL() = %q, want %q", got, testCase.want)
			}
		})
	}
}

func TestJSONOmitsOpenAIBaseForNonCompatibleEndpoints(t *testing.T) {
	ollama := WorkingModel{
		Slug:             "mlvoca",
		BaseURL:          "https://mlvoca.com",
		ChatPath:         "/api/generate",
		ModelID:          "tinyllama",
		OpenAICompatible: false,
	}
	payload, err := json.Marshal(ollama)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, present := decoded["openai_base_url"]; present {
		t.Fatal("an Ollama-shaped endpoint must not advertise an OpenAI base URL")
	}
}

func TestJSONCarriesOpenAIBaseForCompatibleEndpoints(t *testing.T) {
	model := WorkingModel{
		BaseURL:          "https://text.pollinations.ai",
		ChatPath:         "/openai",
		OpenAICompatible: true,
	}
	payload, err := json.Marshal(model)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded["openai_base_url"] != "https://text.pollinations.ai/openai" {
		t.Fatalf("openai_base_url = %v", decoded["openai_base_url"])
	}
}

func TestFirstOpenAICompatibleSkipsIncompatibleEntries(t *testing.T) {
	working := []WorkingModel{
		{Slug: "mlvoca", OpenAICompatible: false},
		{Slug: "pollinations", OpenAICompatible: true},
	}
	first, ok := firstOpenAICompatible(working)
	if !ok || first.Slug != "pollinations" {
		t.Fatalf("got %+v ok=%v, want pollinations", first, ok)
	}
	if _, ok := firstOpenAICompatible(working[:1]); ok {
		t.Fatal("must report none when no entry is OpenAI compatible")
	}
}
