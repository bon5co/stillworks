/* stillworks. Served from the app and loaded through RenderOptions.Scripts: the
   default CSP is default-src 'self', so an inline <script> is dropped.

   Everything here only ever improves on a page that already works without it.

   Sorting and filtering are deliberately not reimplemented. They are URL state
   on the server, which survives a shared link and needs no JavaScript; a second
   sort implementation in the browser could only drift from the first. The row
   drawers are the same: the server writes every one of them into the page, and
   the caret is a link that opens one on a round trip. What this file adds is
   that the round trip is not needed.

   The two calls it makes are the two that cannot be made on the server and
   still mean anything: the runtime API, so "run it here" is our real answer and
   not a screenshot of one, and a provider endpoint from the visitor's own
   address, because keyless quotas are per-IP and a call from our server answers
   a question nobody asked.

   Results are reported in a toaster fixed to the corner. A result printed in
   place would push every row below it down, and this page is a table. */

(function () {
	"use strict";

	// Long enough for a cold model to wake up, short enough that a visitor is
	// not left watching a dead button.
	var TIMEOUT_MS = 20000;
	var ANSWER_LIMIT = 400;
	// How many models the "run it here" toast shows in full. The whole payload
	// is thirty kilobytes of the same shape; three of them plus a count is the
	// part anybody reads, and the toast says how many were left out.
	var SAMPLE_MODELS = 3;

	ready(function () {
		reveal();
		wireTabs();
		wireCopy();
		wireRows();
		wireRunAPI();
		wireTestCalls();
		wireSearchShortcut();
	});

	function ready(run) {
		if (document.readyState === "loading") {
			document.addEventListener("DOMContentLoaded", run);
			return;
		}
		run();
	}

	// ---------- reveal ----------
	// Every control this file drives is written into the HTML with the hidden
	// attribute, so a visitor with no JavaScript is never shown a button that
	// does nothing. The panes stop being a stacked list and become a tab strip
	// at the same moment, for the same reason: the tabs only exist now.
	function reveal() {
		each("[hidden][data-copy-active], [hidden][data-copy-row], [hidden][data-fmt], " +
			"[hidden][data-run-api], [hidden][data-try]", function (element) {
			element.removeAttribute("hidden");
		});
		each(".stack", function (stack) {
			stack.setAttribute("data-tabbed", "");
		});
	}

	function each(selector, run, root) {
		var found = (root || document).querySelectorAll(selector);
		for (var index = 0; index < found.length; index += 1) {
			run(found[index]);
		}
	}

	// ---------- language tabs ----------
	// All the panes share one grid cell, so the card is as tall as its longest
	// snippet and switching languages moves nothing else on the page.
	function wireTabs() {
		document.addEventListener("click", function (event) {
			var tab = closest(event.target, "[data-fmt]");
			if (!tab) {
				return;
			}
			event.preventDefault();
			var snippet = tab.closest(".snippet");
			each("[data-fmt]", function (other) {
				other.setAttribute("aria-selected", String(other === tab));
			}, snippet);
			each(".pane", function (pane) {
				if (pane.getAttribute("data-pane") === tab.getAttribute("data-fmt")) {
					pane.setAttribute("data-active", "");
				} else {
					pane.removeAttribute("data-active");
				}
			}, snippet);
		});
	}

	// ---------- copy ----------
	// The button's label never changes. A "Copy" that becomes "Copied" is about
	// eighteen pixels wider and drags everything beside it sideways; the toast
	// reports it instead.
	function wireCopy() {
		document.addEventListener("click", function (event) {
			var button = closest(event.target, "[data-copy-active], [data-copy-row]");
			if (!button) {
				return;
			}
			event.preventDefault();
			var text = copyTextFor(button);
			if (!text) {
				return;
			}
			copyText(text).then(
				function () {
					toast({
						type: "success",
						title: "Copied to clipboard",
						desc: firstLine(text),
						hold: 2600,
					});
				},
				function () {
					// Never claim it worked. A confirmation over an empty
					// clipboard is worse than an admission.
					toast({
						type: "error",
						title: "Could not reach your clipboard",
						desc: "Select the snippet and copy it yourself.",
						hold: 4000,
					});
				}
			);
		});
	}

	function copyTextFor(button) {
		if (button.hasAttribute("data-copy-row")) {
			// The row's curl, read out of the drawer the server already wrote.
			// Building it here would be a second implementation of every
			// snippet on the site, free to disagree with the first.
			var drawer = document.querySelector(
				'[data-row-detail="' + cssEscape(button.getAttribute("data-copy-row")) + '"]');
			var curl = drawer && drawer.querySelector('[data-pane="curl"] code');
			return curl ? curl.textContent : "";
		}
		var snippet = button.closest(".snippet");
		var active = snippet && snippet.querySelector(".pane[data-active] code");
		return active ? active.textContent : "";
	}

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

	// ---------- rows ----------
	// Anywhere on a row opens it. Buttons and the links inside a row keep their
	// own job -- the endpoint slug is a link to that endpoint's page, and the
	// caret is the no-JavaScript version of this very toggle.
	function wireRows() {
		document.addEventListener("click", function (event) {
			var caret = closest(event.target, "a.caret");
			var row = closest(event.target, "tr.row");
			if (!row) {
				return;
			}
			if (!caret && (closest(event.target, "button") || closest(event.target, "a"))) {
				return;
			}
			// A modified click on the caret is somebody asking for a new tab,
			// and that link is a real address. Let the browser have it.
			if (event.metaKey || event.ctrlKey || event.shiftKey || event.button !== 0) {
				return;
			}
			event.preventDefault();
			toggleRow(row);
		});
	}

	function toggleRow(row) {
		var identifier = row.getAttribute("data-row");
		var detail = document.querySelector('[data-row-detail="' + cssEscape(identifier) + '"]');
		if (!detail) {
			return;
		}
		var opening = detail.hasAttribute("hidden");
		// One at a time, like the server renders it: a page of open drawers is a
		// page with no table on it.
		each("tr.detail", function (other) {
			if (other !== detail) {
				other.setAttribute("hidden", "");
			}
		});
		each("tr.row", function (other) {
			if (other !== row) {
				other.classList.remove("open");
				var caret = other.querySelector("a.caret");
				if (caret) {
					caret.setAttribute("aria-expanded", "false");
				}
			}
		});
		if (opening) {
			detail.removeAttribute("hidden");
		} else {
			detail.setAttribute("hidden", "");
		}
		row.classList.toggle("open", opening);
		var caret = row.querySelector("a.caret");
		if (caret) {
			caret.setAttribute("aria-expanded", String(opening));
		}
		// The address follows the page, so a reload or a copied link lands on
		// the same open row the server would have rendered.
		var parameters = new URLSearchParams(window.location.search);
		if (opening) {
			parameters.set("open", identifier);
		} else {
			parameters.delete("open");
		}
		var query = parameters.toString();
		window.history.replaceState(null, "", query ? "?" + query : window.location.pathname);
	}

	// ---------- run it here ----------
	// Our own API, called from the visitor's browser, with the answer shown as
	// it came back. A canned example would be a screenshot of a promise.
	function wireRunAPI() {
		document.addEventListener("click", function (event) {
			var button = closest(event.target, "[data-run-api]");
			if (!button) {
				return;
			}
			event.preventDefault();
			if (button.getAttribute("aria-busy") === "true") {
				return;
			}
			var url = button.getAttribute("data-run-api");
			busy(button, true);
			var pending = toast({ type: "loading", title: "GET /api/llm/up", desc: hostOf(url) });
			var started = now();
			// Same origin, so the relative path is what actually goes out; the
			// absolute one on the button is the address printed in the snippet
			// beside it, and the two must not drift.
			fetch("/api/llm/up", { headers: { Accept: "application/json" } })
				.then(function (response) {
					return response.text().then(function (text) {
						busy(button, false);
						showAPIResult(pending, response.status, Math.round(now() - started), text);
					});
				})
				.catch(function (error) {
					busy(button, false);
					pending.update({
						type: "error",
						title: "The call did not get through",
						desc: describe(error),
						hold: 8000,
					});
				});
		});
	}

	function showAPIResult(pending, status, elapsed, text) {
		var payload;
		try {
			payload = JSON.parse(text);
		} catch (error) {
			pending.update({
				type: "error",
				title: status + " · that was not JSON",
				code: truncate(text, 600),
				wide: true,
				hold: 12000,
			});
			return;
		}
		var models = payload.models || [];
		var shown = models.slice(0, SAMPLE_MODELS);
		var sample = Object.assign({}, payload, { models: shown });
		var body = JSON.stringify(sample, null, 2);
		if (models.length > shown.length) {
			// Said in the payload rather than silently cut. The count above it
			// is the real one, and a reader has to be able to see that the
			// difference is a display choice and not a shorter answer.
			body = body.replace(/\n\s{4}\}\n\s{2}\]/,
				"\n    },\n    … " + (models.length - shown.length) + " more, same shape\n  ]");
		}
		pending.update({
			type: status >= 200 && status < 300 ? "success" : "error",
			title: status + " · " + models.length + " models · " + elapsed + " ms",
			code: body,
			wide: true,
			hold: 20000,
		});
	}

	// ---------- a test call from the visitor's own address ----------
	function wireTestCalls() {
		document.addEventListener("click", function (event) {
			var button = closest(event.target, "[data-try]");
			if (!button) {
				return;
			}
			event.preventDefault();
			// One call at a time. Several of these providers allow exactly one
			// request in flight per address, so a second press while the first
			// is running earns the 429 the toast is there to explain.
			if (button.getAttribute("aria-busy") === "true") {
				return;
			}
			runDirect(button);
		});
	}

	function runDirect(button) {
		var url = button.getAttribute("data-try");
		var model = button.getAttribute("data-model");
		var shape = button.getAttribute("data-shape");
		busy(button, true);
		var pending = toast({ type: "loading", title: "Calling " + model, desc: hostOf(url) });

		var controller = new AbortController();
		var timer = setTimeout(function () {
			controller.abort();
		}, TIMEOUT_MS);
		var started = now();

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
					busy(button, false);
					showDirect(pending, response.status, Math.round(now() - started), text);
				});
			})
			.catch(function (error) {
				clearTimeout(timer);
				fallBackToServer(button, pending, describe(error));
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

	function showDirect(pending, status, elapsed, text) {
		var answer = extractAnswer(text);
		var good = status >= 200 && status < 300 && answer !== "";
		// The status is the whole point when it is not 200. A 402 or 429 here,
		// against a model this site has verified, is the per-IP quota showing
		// itself, and it is the most useful thing this page can tell anyone.
		pending.update({
			type: good ? "success" : "error",
			title: status + " · " + elapsed + " ms · from your browser",
			desc: answer || refusal(text),
			code: meaning(status),
			hold: good ? 9000 : 14000,
		});
	}

	// Verified against text.pollinations.ai on 2026-08-04: the same request from
	// the same machine answers 200 when spaced out and 402 or 429 when it is
	// not, whether it is sent by curl or by this page, with or without an Origin
	// header, with or without the CORS preflight. Nothing here is a browser
	// problem to fix; it is the per-IP quota this site exists to make visible.
	function meaning(status) {
		if (status === 402) {
			return "402 on a keyless pool means the shared anonymous budget is spent for the moment, " +
				"not that the endpoint wants your card. These pools refill — the same call often " +
				"answers a minute later. The Answered column is our record from our address, not a " +
				"promise about yours.";
		}
		if (status === 429) {
			return "429 is a rate limit. Some of these providers allow only one request in flight per " +
				"address, so a second press while the first is still running earns exactly this.";
		}
		if (status === 401 || status === 403) {
			return "A credential was demanded, or an edge turned the call away. If a row marked " +
				"\"not needed\" keeps answering this, it has stopped being keyless and the shelf is " +
				"wrong until the next probe corrects it.";
		}
		if (status === 404) {
			return "404 means the path is not there. The base URL in this drawer is the one an OpenAI " +
				"client should be given; the bare host without its version prefix answers this.";
		}
		if (status >= 500) {
			return "A 5xx is the provider's own failure. Waiting helps; making the request smaller does not.";
		}
		return "";
	}

	// The browser could not get there. CORS and the page's own security policy
	// both arrive as the same opaque TypeError, so this does not guess which --
	// it asks our server instead, and says in the same breath that the answer is
	// about a different address.
	function fallBackToServer(button, pending, reason) {
		var parameters = new URLSearchParams({
			endpoint: button.getAttribute("data-endpoint") || "",
			model: button.getAttribute("data-model") || "",
		});
		pending.update({
			type: "loading",
			title: "Your browser could not reach it",
			desc: reason + " — asking our server instead…",
		});
		fetch("/api/llm/try?" + parameters.toString(), { headers: { Accept: "application/json" } })
			.then(function (response) {
				return response.json();
			})
			.then(function (payload) {
				busy(button, false);
				var result = payload.result || {};
				if (result.refused) {
					pending.update({
						type: "error",
						title: "Not called",
						desc: result.refused,
						hold: 10000,
					});
					return;
				}
				// Never presented as the visitor's own result. It is a different
				// address, therefore a different claim, and saying so is the
				// entire reason this site exists.
				pending.update({
					type: "error",
					title: "Blocked from your browser · answered from OUR server",
					desc: "HTTP " + result.http_status + " · " + result.latency_ms + " ms · " +
						result.outcome + (result.answer ? " · " + truncate(result.answer, ANSWER_LIMIT) : ""),
					code: "This call was made from " + (result.called_from || "our server") +
						", not from your address, so your own quota is still untested. " +
						"Run the snippet in the drawer to settle it.",
					hold: 14000,
				});
			})
			.catch(function (error) {
				busy(button, false);
				pending.update({
					type: "error",
					title: "Blocked from your browser, and our own route failed too",
					desc: reason + " / " + describe(error),
					hold: 12000,
				});
			});
	}

	// ---------- the search shortcut ----------
	function wireSearchShortcut() {
		document.addEventListener("keydown", function (event) {
			if (event.key !== "/" || event.metaKey || event.ctrlKey || event.altKey) {
				return;
			}
			var active = document.activeElement;
			if (active && (active.tagName === "INPUT" || active.tagName === "TEXTAREA")) {
				return;
			}
			var field = document.getElementById("q");
			if (!field) {
				return;
			}
			event.preventDefault();
			field.focus();
		});
	}

	// ---------- toaster ----------
	var toaster = null;

	function toasterElement() {
		if (toaster) {
			return toaster;
		}
		toaster = document.createElement("div");
		toaster.className = "toaster";
		toaster.setAttribute("role", "status");
		toaster.setAttribute("aria-live", "polite");
		document.body.appendChild(toaster);
		return toaster;
	}

	var GLYPH = {
		success: "tick",
		error: "cross",
		loading: "spin",
	};

	function toast(options) {
		var element = document.createElement("div");
		element.className = "toast";
		var container = toasterElement();
		container.appendChild(element);
		while (container.children.length > 4) {
			container.firstChild.remove();
		}

		var timer = null;

		function dismiss() {
			clearTimeout(timer);
			element.classList.add("leaving");
			setTimeout(function () {
				element.remove();
			}, 180);
		}

		// Built from nodes with textContent rather than from a string of HTML.
		// Everything in a toast is somebody else's: a provider's error body, a
		// model's own words, a JSON payload. None of it is ours to trust, and
		// innerHTML would hand a provider a script tag on our origin.
		function paint(next) {
			element.classList.toggle("wide", Boolean(next.wide));
			element.textContent = "";

			var slot = document.createElement("span");
			slot.className = "glyph-slot";
			var glyph = document.createElement("span");
			glyph.className = GLYPH[next.type] || GLYPH.success;
			slot.appendChild(glyph);
			element.appendChild(slot);

			var body = document.createElement("span");
			body.className = "body";
			body.appendChild(span("title", next.title));
			if (next.desc) {
				body.appendChild(span("desc", next.desc));
			}
			if (next.code) {
				var payload = document.createElement("pre");
				payload.className = "payload";
				payload.textContent = next.code;
				body.appendChild(payload);
			}
			element.appendChild(body);

			if (next.type !== "loading") {
				var close = document.createElement("button");
				close.type = "button";
				close.className = "dismiss";
				close.setAttribute("aria-label", "Dismiss");
				close.textContent = "×";
				close.addEventListener("click", dismiss);
				element.appendChild(close);
			}

			clearTimeout(timer);
			if (next.type !== "loading") {
				timer = setTimeout(dismiss, next.hold || 5000);
			}
		}

		paint(options);
		return { update: paint, dismiss: dismiss };
	}

	function span(className, text) {
		var element = document.createElement("span");
		element.className = className;
		element.textContent = text;
		return element;
	}

	// ---------- odds and ends ----------
	function busy(button, running) {
		if (running) {
			button.setAttribute("aria-busy", "true");
		} else {
			button.removeAttribute("aria-busy");
		}
	}

	function closest(node, selector) {
		return node && node.closest ? node.closest(selector) : null;
	}

	// A row identifier is an endpoint slug and a model id, and model ids carry
	// dots, colons and slashes. CSS.escape is the only correct way to put one
	// inside an attribute selector; the fallback quotes the two characters that
	// would end the selector early.
	function cssEscape(value) {
		if (window.CSS && window.CSS.escape) {
			return window.CSS.escape(value);
		}
		return String(value).replace(/["\\]/g, "\\$&");
	}

	function hostOf(url) {
		return String(url).replace(/^https?:\/\//, "").split("/")[0];
	}

	function firstLine(text) {
		var line = String(text).split("\n")[0];
		return line.length > 54 ? line.slice(0, 53) + "…" : line;
	}

	function now() {
		return window.performance && window.performance.now ? window.performance.now() : Date.now();
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

	// String(), because everything reaching here came out of somebody else's
	// JSON and none of it is promised to be text. A body shaped {"response":{}}
	// made .trim() throw inside the .then, which the trailing .catch read as
	// "the browser could not reach it" and escalated to the server route -- so a
	// provider could make every visitor's browser quietly spend this server's
	// rate-limited quota by answering with an object where a string belonged.
	function truncate(value, limit) {
		var trimmed = String(value == null ? "" : value).trim().replace(/\s+/g, " ");
		return trimmed.length > limit ? trimmed.slice(0, limit) + "…" : trimmed;
	}
})();
