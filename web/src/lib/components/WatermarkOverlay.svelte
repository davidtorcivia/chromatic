<script lang="ts">
    import { onMount, onDestroy } from "svelte";

    interface Props {
        mode: "none" | "text" | "logo" | "both";
        text?: string;
        logoUrl?: string;
        logoPosition?:
            | "top-left"
            | "top-right"
            | "bottom-left"
            | "bottom-right";
        opacity?: number;
        participantName: string;
    }

    let {
        mode = "none",
        text,
        logoUrl,
        logoPosition = "bottom-right",
        opacity = 0.3,
        participantName,
    }: Props = $props();

    let canvasEl: HTMLCanvasElement;
    let observer: MutationObserver;
    let animationFrame: number;

    // Template variable replacement
    function processText(template: string): string {
        const now = new Date();
        return template
            .replace(/\{\{name\}\}/g, participantName)
            .replace(/\{\{date\}\}/g, now.toISOString().slice(0, 10))
            .replace(
                /\{\{time\}\}/g,
                now.toLocaleTimeString([], {
                    hour: "2-digit",
                    minute: "2-digit",
                }),
            );
    }

    function draw() {
        if (!canvasEl) return;

        const ctx = canvasEl.getContext("2d");
        if (!ctx) return;

        const parent = canvasEl.parentElement;
        if (!parent) return;

        // Match parent size
        canvasEl.width = parent.clientWidth;
        canvasEl.height = parent.clientHeight;

        ctx.clearRect(0, 0, canvasEl.width, canvasEl.height);
        ctx.globalAlpha = opacity;

        // Draw text watermark
        if ((mode === "text" || mode === "both") && text) {
            const processedText = processText(text);
            ctx.font = "14px Inter, sans-serif";
            ctx.fillStyle = "white";
            ctx.textAlign = "center";
            ctx.textBaseline = "middle";
            ctx.shadowColor = "rgba(0, 0, 0, 0.8)";
            ctx.shadowBlur = 2;

            // Center position
            ctx.fillText(
                processedText,
                canvasEl.width / 2,
                canvasEl.height / 2,
            );
        }

        // Draw logo watermark
        // Note: In production, load and cache the logo image

        animationFrame = requestAnimationFrame(draw);
    }

    function setupTamperDetection() {
        if (!canvasEl) return;

        observer = new MutationObserver((mutations) => {
            for (const mutation of mutations) {
                if (mutation.type === "attributes") {
                    const attr = mutation.attributeName;
                    if (attr === "style" || attr === "class") {
                        // Check for visibility/opacity tampering
                        const style = window.getComputedStyle(canvasEl);
                        if (
                            style.display === "none" ||
                            style.visibility === "hidden" ||
                            parseFloat(style.opacity) < 0.1
                        ) {
                            handleTampering();
                        }
                    }
                } else if (mutation.type === "childList") {
                    // Check if canvas was removed
                    if (!document.contains(canvasEl)) {
                        handleTampering();
                    }
                }
            }
        });

        // Watch the canvas and its parent
        observer.observe(canvasEl.parentElement!, {
            childList: true,
            subtree: true,
            attributes: true,
            attributeFilter: ["style", "class"],
        });

        observer.observe(canvasEl, {
            attributes: true,
            attributeFilter: ["style", "class"],
        });
    }

    function handleTampering() {
        console.error("Watermark tampering detected");

        // Emit event for parent to handle
        window.dispatchEvent(
            new CustomEvent("chromatic:tampering", {
                detail: { type: "watermark" },
            }),
        );
    }

    onMount(() => {
        if (mode !== "none") {
            draw();
            setupTamperDetection();
        }
    });

    onDestroy(() => {
        if (animationFrame) {
            cancelAnimationFrame(animationFrame);
        }
        if (observer) {
            observer.disconnect();
        }
    });
</script>

{#if mode !== "none"}
    <canvas bind:this={canvasEl} class="watermark-canvas"></canvas>
{/if}

<style>
    .watermark-canvas {
        position: absolute;
        inset: 0;
        width: 100%;
        height: 100%;
        pointer-events: none;
        z-index: 10;
    }
</style>
