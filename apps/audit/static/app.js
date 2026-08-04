/* stillworks. Served from the app and loaded through RenderOptions.Scripts: the
   default CSP is default-src 'self', so an inline <script> is dropped.

   This file does exactly one thing, and only ever improves on a page that
   already works without it. Every test-call button is a real GET form pointing
   at /llm/try, which runs the call from this server. That answers a weaker
   question than the one worth asking: keyless quotas are per-IP, so what a
   visitor needs to know is whether the endpoint answers *them*. So when the
   script is running it makes the same call from the visitor's own browser, and
   falls back to the server route -- clearly labelled -- when the browser cannot
   get there.

   Sorting and filtering are deliberately not reimplemented here. They are URL
   state on the server, which survives a shared link and needs no JavaScript;
   a second sort implementation in the browser could only drift from the first. */

(function () {
	"use strict";

	// Long enough for a cold model to wake up, short enough that a visitor is
	// not left watching a dead button.
	var TIMEOUT_MS = 20000;
	var ANSWER_LIMIT = 600;

	document.addEventListener("submit", function (event) {
		var form = event.target;
		if (!form.classList || !form.classList.contains("try")) {
			return;
		}
		var url = form.getAttribute("data-url");
		var model = form.getAttribute("data-model");
		if (!url || !model) {
			// No attributes means no direct call is possible; let the form
			// submit to the server route as it would with no script at all.
			return;
		}
		event.preventDefault();
		runDirect(form, url, model, form.getAttribute("data-shape"));
	});

	function runDirect(form, url, model, shape) {
		var panel = resultPanel(form);
		var button = form.querySelector("button");
		if (button) {
			button.disabled = true;
		}
		panel.className = "try-result muted";
		panel.textContent = "Calling " + url + " from your browser…";

		var controller = new AbortController();
		var timer = setTimeout(function () {
			controller.abort();
		}, TIMEOUT_MS);
		var started = performance.now();

		fetch(url, {
			method: "POST",
			headers: { "Content-Type": "application/json" },
			body: JSON.stringify(requestBody(model, shape)),
			signal: controller.signal,
			// No credentials, and no Authorization header: the claim under test
			// is that this works with nothing at all.
			credentials: "omit",
			mode: "cors",
		})
			.then(function (response) {
				return response.text().then(function (text) {
					clearTimeout(timer);
					showDirect(panel, response.status, Math.round(performance.now() - started), text);
					release(button);
				});
			})
			.catch(function (error) {
				clearTimeout(timer);
				fallBackToServer(form, panel, button, describe(error));
			});
	}

	function requestBody(model, shape) {
		var prompt = "Say hello in five words.";
		if (shape === "ollama") {
			return { model: model, prompt: prompt, stream: false };
		}
		return {
			model: model,
			messages: [{ role: "user", content: prompt }],
			max_tokens: 96,
			stream: false,
		};
	}

	function showDirect(panel, status, elapsed, text) {
		var answer = extractAnswer(text);
		var good = status >= 200 && status < 300 && answer !== "";
		panel.className = good ? "try-result ok" : "try-result bad";
		// The status is the whole point when it is not 200: a 402 or 429 here,
		// against a model this site has verified, is the per-IP quota showing
		// itself, and it is the most useful thing this page can tell anyone.
		panel.textContent =
			"HTTP " + status + " · " + elapsed + " ms · from your browser at " + clockTime() +
			(answer ? " · " + answer : " · " + refusal(text));
	}

	function fallBackToServer(form, panel, button, reason) {
		var parameters = new URLSearchParams(new FormData(form));
		panel.className = "try-result muted";
		panel.textContent = "Your browser could not reach it (" + reason + "). Asking our server instead…";

		fetch("/api/llm/try?" + parameters.toString(), { headers: { Accept: "application/json" } })
			.then(function (response) {
				return response.json();
			})
			.then(function (payload) {
				var result = payload.result || {};
				panel.className = "try-result warn";
				if (result.refused) {
					panel.textContent = "Not called: " + result.refused;
					release(button);
					return;
				}
				// Never presented as the visitor's own result. It is a different
				// address, therefore a different claim, and saying so is the
				// entire reason this site exists.
				panel.textContent =
					"Blocked from your browser (" + reason + "). From OUR server instead: HTTP " +
					result.http_status + " · " + result.latency_ms + " ms · " + result.outcome +
					" · at " + result.called_at +
					(result.answer ? " · " + truncate(result.answer, ANSWER_LIMIT) : "") +
					" — this is not your address, so your own quota is still untested.";
				release(button);
			})
			.catch(function (error) {
				panel.className = "try-result bad";
				panel.textContent =
					"Blocked from your browser (" + reason + "), and our own server route failed too (" +
					describe(error) + ").";
				release(button);
			});
	}

	// The result goes in a row of its own under the one that was clicked, not
	// inside the button's cell: a status line and an answer squeezed into the
	// narrowest column in the table is unreadable, which defeats the point of
	// showing it.
	function resultPanel(form) {
		var row = form.closest("tr");
		if (!row) {
			return inlinePanel(form);
		}
		if (row.nextElementSibling && row.nextElementSibling.classList.contains("try-row")) {
			return row.nextElementSibling.querySelector(".try-result");
		}
		var resultRow = document.createElement("tr");
		resultRow.className = "try-row";
		var cell = document.createElement("td");
		cell.colSpan = row.children.length;
		var panel = document.createElement("div");
		panel.className = "try-result muted";
		cell.appendChild(panel);
		resultRow.appendChild(cell);
		row.parentNode.insertBefore(resultRow, row.nextSibling);
		return panel;
	}

	function inlinePanel(form) {
		var existing = form.querySelector(".try-result");
		if (existing) {
			return existing;
		}
		var panel = document.createElement("div");
		panel.className = "try-result muted";
		form.appendChild(panel);
		return panel;
	}

	function release(button) {
		if (button) {
			button.disabled = false;
		}
	}

	// extractAnswer mirrors the shapes the Go prober already tolerates: the
	// OpenAI envelope, a bare text choice, a reasoning-only reply, and Ollama's
	// {"response": ...}.
	function extractAnswer(text) {
		var payload;
		try {
			payload = JSON.parse(text);
		} catch (error) {
			return "";
		}
		if (payload.response) {
			return truncate(payload.response, ANSWER_LIMIT);
		}
		var choices = payload.choices || [];
		for (var index = 0; index < choices.length; index += 1) {
			var choice = choices[index];
			var message = choice.message || {};
			var content = message.content || choice.text || message.reasoning || "";
			if (content) {
				return truncate(content, ANSWER_LIMIT);
			}
		}
		return "";
	}

	// describe turns a fetch rejection into something a person can act on. The
	// browser deliberately gives JavaScript no detail about a CORS or CSP
	// refusal -- both arrive as the same opaque TypeError -- so this says what
	// it can and does not invent a cause.
	function describe(error) {
		if (error && error.name === "AbortError") {
			return "no answer within " + TIMEOUT_MS / 1000 + " s";
		}
		if (error && error.name === "TypeError") {
			return "blocked by CORS or the page's security policy, or the host was unreachable";
		}
		return (error && error.message) || "unknown error";
	}

	// refusal digs the message out of an error body. Taking the first line
	// instead would show "{" for every pretty-printed JSON error, which is the
	// case that matters most: a 402 or 429 from the visitor's own address is the
	// single most useful thing this button can report.
	function refusal(text) {
		var payload;
		try {
			payload = JSON.parse(text);
		} catch (error) {
			return truncate(text, 300) || "empty response";
		}
		var error = payload.error || payload;
		var message = error.message || error.detail || payload.message || payload.detail;
		if (typeof error === "string") {
			message = error;
		}
		return truncate(message || text, 300) || "empty response";
	}

	function truncate(value, limit) {
		var trimmed = (value || "").trim().replace(/\s+/g, " ");
		return trimmed.length > limit ? trimmed.slice(0, limit) + "…" : trimmed;
	}

	function clockTime() {
		return new Date().toISOString().replace("T", " ").slice(0, 19) + " UTC";
	}
})();
