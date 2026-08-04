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

	// Copy buttons. They are written into the HTML hidden and revealed here, so
	// a visitor with no JavaScript sees no control that does nothing -- the
	// snippet is still there, still selectable, and no longer clipped.
	revealCopyButtons();
	document.addEventListener("DOMContentLoaded", revealCopyButtons);

	function revealCopyButtons() {
		var buttons = document.querySelectorAll("button.copy[hidden]");
		for (var index = 0; index < buttons.length; index += 1) {
			buttons[index].removeAttribute("hidden");
		}
	}

	document.addEventListener("click", function (event) {
		var button = event.target.closest && event.target.closest("button.copy");
		if (!button) {
			return;
		}
		event.preventDefault();
		var source = document.getElementById(button.getAttribute("data-copy"));
		if (!source) {
			return;
		}
		copyText(source.textContent).then(
			function () {
				flash(button, "Copied");
			},
			function () {
				// Never claim it worked. A button that says Copied over an empty
				// clipboard is worse than one that admits it could not.
				flash(button, "Select and copy");
			}
		);
	});

	// The async clipboard API needs a secure context, which excludes a plain
	// HTTP deployment and some embedded browsers; execCommand is the fallback
	// that still works there. Both are same-origin, and neither reads anything.
	function copyText(text) {
		if (navigator.clipboard && window.isSecureContext) {
			return navigator.clipboard.writeText(text);
		}
		return new Promise(function (resolve, reject) {
			var field = document.createElement("textarea");
			field.value = text;
			field.setAttribute("readonly", "");
			field.className = "offscreen-copy";
			document.body.appendChild(field);
			field.select();
			var copied = false;
			try {
				copied = document.execCommand("copy");
			} catch (error) {
				copied = false;
			}
			document.body.removeChild(field);
			copied ? resolve() : reject(new Error("copy refused"));
		});
	}

	function flash(button, message) {
		var original = "Copy " + (button.getAttribute("data-label") || "");
		button.textContent = message;
		button.setAttribute("data-copied", "");
		setTimeout(function () {
			button.textContent = original.trim();
			button.removeAttribute("data-copied");
		}, 1600);
	}

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
		// One call at a time. Several of these providers allow exactly one
		// request in flight per address, so a second press while the first is
		// running earns the 429 the panel is there to explain.
		var button = form.querySelector("button");
		if (button && button.hasAttribute("data-busy")) {
			return;
		}
		runDirect(form, url, model, form.getAttribute("data-shape"));
	});

	function runDirect(form, url, model, shape) {
		var panel = resultPanel(form);
		var button = form.querySelector("button");
		lock(button);
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
					showDirect(panel, response.status, Math.round(performance.now() - started), text, form, url, model, shape);
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

	function showDirect(panel, status, elapsed, text, form, url, model, shape) {
		var answer = extractAnswer(text);
		var good = status >= 200 && status < 300 && answer !== "";
		panel.className = good ? "try-result ok" : "try-result bad";
		panel.textContent = "";
		// The status is the whole point when it is not 200: a 402 or 429 here,
		// against a model this site has verified, is the per-IP quota showing
		// itself, and it is the most useful thing this page can tell anyone.
		panel.appendChild(
			line(
				"HTTP " + status + " · " + elapsed + " ms · from your browser at " + clockTime() +
					(answer ? " · " + answer : " · " + refusal(text))
			)
		);
		// What the status means, and what to do about it. The panel used to
		// print the number alone, so a visitor who pressed the button on a row
		// reading 17/17 was shown "402" and left to conclude the shelf lies.
		var explanation = meaning(status);
		if (explanation) {
			panel.appendChild(muted(explanation));
		}
		panel.appendChild(actions(form, url, model, shape, status));
	}

	// Verified against text.pollinations.ai on 2026-08-04: the same request from
	// the same machine answers 200 when spaced out and 402 or 429 when it is
	// not, whether it is sent by curl or by this page, with or without an Origin
	// header, with or without the CORS preflight. Nothing here is a browser
	// problem to fix; it is the per-IP quota this site exists to make visible,
	// and the only defect was that the page never said so.
	function meaning(status) {
		if (status === 402) {
			return "402 on a keyless pool means the shared anonymous budget is spent for the moment, " +
				"not that the endpoint wants your card. These pools refill — the same call often " +
				"answers a minute later. The Worked column is our record from our address, not a " +
				"promise about yours.";
		}
		if (status === 429) {
			return "429 is a rate limit. Some of these providers allow only one request in flight per " +
				"address, so a second press while the first is still running earns exactly this.";
		}
		if (status === 401 || status === 403) {
			return "A credential was demanded, or an edge turned the call away. If a keyless endpoint " +
				"keeps answering this, it has stopped being keyless and the shelf is wrong until the " +
				"next probe corrects it.";
		}
		if (status === 404) {
			return "404 means the path is not there. The base URL shown on this row is the one an " +
				"OpenAI client should be given; the bare host without its version prefix answers this.";
		}
		if (status >= 500) {
			return "A 5xx is the provider's own failure. Waiting helps; making the request smaller does not.";
		}
		return "";
	}

	// Every result ends in something to do. It used to end in a clipped
	// sentence in a colspan cell with no link and no button on it at all.
	function actions(form, url, model, shape, status) {
		var row = document.createElement("div");
		row.className = "try-actions";

		var again = document.createElement("button");
		again.type = "button";
		again.className = "copy";
		again.textContent = status === 429 || status === 402 ? "Try again" : "Call again";
		// Disabled the moment it is pressed. The result panel is a sibling row
		// outside the form, so this button escapes the guard that disables the
		// form's own button while a call is in flight -- and several of these
		// providers allow exactly one request in flight per address, which is the
		// 429 the panel right above it is there to explain. A retry button that
		// manufactures the error it is offering to retry is worse than none.
		again.addEventListener("click", function () {
			again.disabled = true;
			form.dispatchEvent(new Event("submit", { bubbles: true, cancelable: true }));
		});
		row.appendChild(again);

		var copy = document.createElement("button");
		copy.type = "button";
		copy.className = "copy";
		copy.setAttribute("data-label", "as curl");
		copy.textContent = "Copy as curl";
		copy.addEventListener("click", function () {
			copyText(curlFor(url, model, shape)).then(
				function () {
					flash(copy, "Copied");
				},
				function () {
					flash(copy, "Copy refused");
				}
			);
		});
		row.appendChild(copy);

		var slug = form.querySelector("input[name=endpoint]");
		if (slug) {
			var evidence = document.createElement("a");
			evidence.href = "/llm/" + encodeURIComponent(slug.value);
			evidence.textContent = "Our probe log for " + slug.value;
			row.appendChild(evidence);
		}

		var shelf = document.createElement("a");
		shelf.href = "/llm/";
		shelf.textContent = "Try another model";
		row.appendChild(shelf);
		return row;
	}

	// The exact call the browser just made, as a shell command. Same body, same
	// URL, no invented headers: somebody who does not believe the result has to
	// be able to reproduce it rather than reconstruct it.
	// Both interpolations are quoted and escaped. The body was already; the URL
	// was not, and this string goes to a clipboard and from there into somebody
	// else's shell. It is not reachable today -- base_url and chat_path are only
	// ever written from the in-repo seed list, never from a provider's response
	// -- but "not reachable today" is one bad seed row away from running a
	// command on a reader's machine, and quoting it costs nothing.
	function curlFor(url, model, shape) {
		return (
			"curl " + shellQuote(url) + " \\\n" +
			"  -H 'Content-Type: application/json' \\\n" +
			"  -d " + shellQuote(JSON.stringify(requestBody(model, shape)))
		);
	}

	function shellQuote(value) {
		return "'" + String(value).replace(/'/g, "'\"'\"'") + "'";
	}

	function line(text) {
		var element = document.createElement("div");
		element.textContent = text;
		return element;
	}

	function muted(text) {
		var element = document.createElement("div");
		element.className = "try-note";
		element.textContent = text;
		return element;
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
	// A live region, so the result of the button a visitor just pressed is
	// announced rather than only drawn. Without it a screen-reader user gets no
	// signal at all that the call happened, on the diff's flagship interaction.
	function newResultPanel() {
		var panel = document.createElement("div");
		panel.className = "try-result muted";
		panel.setAttribute("role", "status");
		panel.setAttribute("aria-live", "polite");
		return panel;
	}

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
		var panel = newResultPanel();
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
		var panel = newResultPanel();
		form.appendChild(panel);
		return panel;
	}

	// lock and release stop a second call while one is in flight without using
	// `disabled`. A disabled button is removed from the tab order, so the
	// browser drops focus to &lt;body&gt; and a keyboard visitor's place in the page
	// is lost at exactly the moment something they asked for is happening. The
	// label carries the state instead, aria-busy says so, and a data flag does
	// the actual guarding.
	function lock(button) {
		if (!button) {
			return;
		}
		if (!button.hasAttribute("data-label-idle")) {
			button.setAttribute("data-label-idle", button.textContent.trim());
		}
		button.setAttribute("data-busy", "");
		button.setAttribute("aria-busy", "true");
		button.textContent = "Calling…";
	}

	function release(button) {
		if (!button) {
			return;
		}
		button.removeAttribute("data-busy");
		button.removeAttribute("aria-busy");
		button.textContent = button.getAttribute("data-label-idle") || "Test call";
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

	// String(), because everything reaching here came out of somebody else's JSON
	// and none of it is promised to be text. A body shaped {"response":{}} or
	// {"error":{"message":{}}} made .trim() throw inside the .then, which the
	// trailing .catch read as "the browser could not reach it" and escalated to
	// the server route -- so a provider could make every visitor's browser
	// quietly spend this server's rate-limited quota by answering with an object
	// where a string was expected.
	function truncate(value, limit) {
		var trimmed = String(value == null ? "" : value).trim().replace(/\s+/g, " ");
		return trimmed.length > limit ? trimmed.slice(0, limit) + "…" : trimmed;
	}

	function clockTime() {
		return new Date().toISOString().replace("T", " ").slice(0, 19) + " UTC";
	}
})();
