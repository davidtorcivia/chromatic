/**
 * Glass tooltip action, replacing native title= bubbles (which look like
 * the browser, not the app). Usage: `use:tooltip={"Mute (M)"}`.
 *
 * Shows above the element after a short hover/focus delay; hides on
 * leave, blur, press, or scroll. Styled by .glass-tooltip in app.css.
 */

const SHOW_DELAY_MS = 450;

export function tooltip(node: HTMLElement, text: string) {
	let tip: HTMLDivElement | null = null;
	let timer: ReturnType<typeof setTimeout> | null = null;
	let current = text;

	function show() {
		timer = null;
		if (tip || !current) return;
		tip = document.createElement("div");
		tip.className = "glass-tooltip";
		tip.setAttribute("role", "tooltip");
		tip.textContent = current;
		document.body.appendChild(tip);
		const rect = node.getBoundingClientRect();
		tip.style.left = `${rect.left + rect.width / 2}px`;
		tip.style.top = `${rect.top - 8}px`;
		// Next frame so the entrance transition runs
		requestAnimationFrame(() => tip?.classList.add("visible"));
	}

	function hide() {
		if (timer) {
			clearTimeout(timer);
			timer = null;
		}
		tip?.remove();
		tip = null;
	}

	function schedule() {
		if (timer || tip) return;
		timer = setTimeout(show, SHOW_DELAY_MS);
	}

	node.addEventListener("pointerenter", schedule);
	node.addEventListener("pointerleave", hide);
	node.addEventListener("pointerdown", hide);
	node.addEventListener("focus", schedule);
	node.addEventListener("blur", hide);
	window.addEventListener("scroll", hide, true);

	return {
		update(newText: string) {
			current = newText;
			if (tip) tip.textContent = newText;
		},
		destroy() {
			hide();
			node.removeEventListener("pointerenter", schedule);
			node.removeEventListener("pointerleave", hide);
			node.removeEventListener("pointerdown", hide);
			node.removeEventListener("focus", schedule);
			node.removeEventListener("blur", hide);
			window.removeEventListener("scroll", hide, true);
		},
	};
}
