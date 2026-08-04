package audit

import (
	"net/url"
	"slices"
	"testing"
)

func keyless(value bool) *bool { return &value }

func TestApplicableCapabilitiesAsksOnlyWhatIsWorthAsking(t *testing.T) {
	openai := Endpoint{Slug: "llm7", OpenAICompatible: true}
	withImages := Endpoint{Slug: "ovh-anonymous", OpenAICompatible: true, ImageMode: ImageModeOpenAI}
	ollama := Endpoint{Slug: "mlvoca", OpenAICompatible: false}

	cases := []struct {
		name     string
		endpoint Endpoint
		model    Model
		want     []string
	}{
		{
			name:     "a model proven keyless gets the full chat set",
			endpoint: openai,
			model:    Model{ModelID: "gpt-oss:20b", ChatCapable: true, Keyless: keyless(true)},
			want:     ChatCapabilities,
		},
		{
			// Four 401s a day on somebody's paid tier is noise on their logs
			// and ours, and proves nothing we do not already know.
			name:     "a model that demands a key is not asked about features",
			endpoint: openai,
			model:    Model{ModelID: "claude-fable-5", ChatCapable: true, Keyless: keyless(false)},
			want:     nil,
		},
		{
			name:     "a model whose keylessness is unknown waits its turn",
			endpoint: openai,
			model:    Model{ModelID: "brand-new", ChatCapable: true},
			want:     nil,
		},
		{
			// OVH publishes no modalities, so the name is all there is.
			name:     "an image model is asked only whether it draws",
			endpoint: withImages,
			model:    Model{ModelID: "stable-diffusion-xl-base-v10", ChatCapable: false},
			want:     []string{CapabilityImageOut},
		},
		{
			// A provider that lists one id in both its text and its image
			// listing has a model that does both. Asking it only about drawing
			// would lose the other four answers.
			name:     "a model in both listings is asked everything",
			endpoint: withImages,
			model: Model{
				ModelID: "omni", ChatCapable: true, OutputModes: "image", Keyless: keyless(true),
			},
			want: append(append([]string{}, ChatCapabilities...), CapabilityImageOut),
		},
		{
			name:     "an Ollama-shaped endpoint has no capability surface to ask about",
			endpoint: ollama,
			model:    Model{ModelID: "tinyllama", ChatCapable: true, Keyless: keyless(true)},
			want:     nil,
		},
		{
			// llm7 lists whisper and embeddings beside its chat models; a chat
			// capability probe against one is a guaranteed 4xx.
			name:     "a non-chat model on an endpoint with no image surface is skipped",
			endpoint: openai,
			model:    Model{ModelID: "whisper-large-v3", ChatCapable: false, Keyless: keyless(true)},
			want:     nil,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := applicableCapabilities(testCase.endpoint, testCase.model)
			if !slices.Equal(got, testCase.want) {
				t.Fatalf("got %v, want %v", got, testCase.want)
			}
		})
	}
}

func TestProducesImagesPrefersPublishedModalities(t *testing.T) {
	// Pollinations' image listing tells us the modality outright.
	if !producesImages(Model{ModelID: "sana", OutputModes: "image"}) {
		t.Fatal("a published image modality must decide it")
	}
	// OVH publishes nothing, so the name is the only evidence.
	for _, id := range []string{"stable-diffusion-xl-base-v10", "stabilityai/stable-diffusion-xl-base-1.0", "gpt-image-2"} {
		if !producesImages(Model{ModelID: id}) {
			t.Fatalf("%q should be probed for image generation", id)
		}
	}
	if producesImages(Model{ModelID: "Mistral-7B-Instruct-v0.3", ChatCapable: true, OutputModes: "text"}) {
		t.Fatal("a chat model must not be asked to draw")
	}
	// Named like an image model, but the provider says it chats: the published
	// fact wins, the same way it does for chat_capable.
	if producesImages(Model{ModelID: "vision-image-chat", ChatCapable: true, OutputModes: "text"}) {
		t.Fatal("a model the provider calls a chat model must not be asked to draw")
	}
}

func TestRequestedFeatures(t *testing.T) {
	cases := []struct {
		name    string
		query   string
		want    []string
		wantErr bool
	}{
		{"nothing asked for", "", nil, false},
		{"one feature", "feature=tools", []string{CapabilityTools}, false},
		{"comma separated", "feature=tools,json_schema", []string{CapabilityTools, CapabilityJSONSchema}, false},
		{"repeated parameter", "feature=tools&feature=vision", []string{CapabilityTools, CapabilityVision}, false},
		{"whitespace and case", "feature=%20Tools%20,%20VISION", []string{CapabilityTools, CapabilityVision}, false},
		{"the same one twice", "feature=tools,tools", []string{CapabilityTools}, false},
		// A filter that quietly matches nothing would read as a provider outage.
		{"a name that does not exist", "feature=tols", nil, true},
		{"a feature that is not a capability", "feature=streaming", nil, true},
		// No model both draws and chats, so this can only ever return nothing.
		{"drawing and chatting at once", "feature=image_out,tools", nil, true},
		{"drawing alone", "feature=image_out", []string{CapabilityImageOut}, false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			query, err := url.ParseQuery(testCase.query)
			if err != nil {
				t.Fatalf("ParseQuery: %v", err)
			}
			got, err := requestedFeatures(query)
			if testCase.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("requestedFeatures: %v", err)
			}
			if !slices.Equal(got, testCase.want) {
				t.Fatalf("got %v, want %v", got, testCase.want)
			}
		})
	}
}

func TestEveryPublishedCapabilityHasAProbe(t *testing.T) {
	for _, capability := range Capabilities {
		if !KnownCapability(capability) {
			t.Fatalf("%q is published but not recognised", capability)
		}
		if capability == CapabilityImageOut {
			continue // probed through the image surface, not the chat body
		}
		if _, err := capabilityBody("any-model", capability); err != nil {
			t.Fatalf("published capability %q has no probe: %v", capability, err)
		}
	}
	if KnownCapability("telepathy") {
		t.Fatal("an unpublished name must not be accepted")
	}
}
