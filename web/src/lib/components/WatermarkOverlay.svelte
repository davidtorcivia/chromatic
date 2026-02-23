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
        roomName?: string;
        logoSize?: number; // Size in pixels (default 80)
        logoPadding?: number; // Padding from edges (default 20)
    }

    let {
        mode = "none",
        text,
        logoUrl,
        logoPosition = "bottom-right",
        opacity = 0.3,
        participantName,
        roomName = "",
        logoSize = 80,
        logoPadding = 20,
    }: Props = $props();

    let canvasEl = $state<HTMLCanvasElement | null>(null);
    let observer: MutationObserver;
    let animationFrame: number;
    let logoImage: HTMLImageElement | null = null;
    let logoLoaded = false;

    // Load and cache logo image
    function loadLogo(url: string): Promise<HTMLImageElement> {
        return new Promise((resolve, reject) => {
            const img = new Image();
            img.crossOrigin = "anonymous";
            img.onload = () => resolve(img);
            img.onerror = () => reject(new Error(`Failed to load logo: ${url}`));
            img.src = url;
        });
    }

    // Template variable replacement
    function processText(template: string): string {
        const now = new Date();
        return template
            .replace(/\{\{\s*name\s*\}\}/g, participantName)
            .replace(/\{\{\s*room\s*\}\}/g, roomName)
            .replace(/\{\{\s*date\s*\}\}/g, now.toISOString().slice(0, 10))
            .replace(
                /\{\{\s*time\s*\}\}/g,
                now.toLocaleTimeString([], {
                    hour: "2-digit",
                    minute: "2-digit",
                }),
            );
    }

    // Calculate logo position based on logoPosition prop
    function getLogoPosition(
        canvasWidth: number,
        canvasHeight: number,
        imgWidth: number,
        imgHeight: number
    ): { x: number; y: number } {
        // Scale image to fit within logoSize while maintaining aspect ratio
        const scale = Math.min(logoSize / imgWidth, logoSize / imgHeight);
        const scaledWidth = imgWidth * scale;
        const scaledHeight = imgHeight * scale;

        let x: number;
        let y: number;

        switch (logoPosition) {
            case "top-left":
                x = logoPadding;
                y = logoPadding;
                break;
            case "top-right":
                x = canvasWidth - scaledWidth - logoPadding;
                y = logoPadding;
                break;
            case "bottom-left":
                x = logoPadding;
                y = canvasHeight - scaledHeight - logoPadding;
                break;
            case "bottom-right":
            default:
                x = canvasWidth - scaledWidth - logoPadding;
                y = canvasHeight - scaledHeight - logoPadding;
                break;
        }

        return { x, y };
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

        // Draw text watermark (bottom-right corner)
        if ((mode === "text" || mode === "both") && text) {
            const processedText = processText(text);
            ctx.font = "14px 'Work Sans', sans-serif";
            ctx.fillStyle = "white";
            ctx.textAlign = "right";
            ctx.textBaseline = "bottom";
            ctx.shadowColor = "rgba(0, 0, 0, 0.8)";
            ctx.shadowBlur = 2;

            ctx.fillText(
                processedText,
                canvasEl.width - logoPadding,
                canvasEl.height - logoPadding,
            );
        }

        // Draw logo watermark
        if ((mode === "logo" || mode === "both") && logoImage && logoLoaded) {
            const scale = Math.min(
                logoSize / logoImage.width,
                logoSize / logoImage.height
            );
            const scaledWidth = logoImage.width * scale;
            const scaledHeight = logoImage.height * scale;

            const pos = getLogoPosition(
                canvasEl.width,
                canvasEl.height,
                logoImage.width,
                logoImage.height
            );

            // Add subtle shadow for visibility on any background
            ctx.shadowColor = "rgba(0, 0, 0, 0.5)";
            ctx.shadowBlur = 4;
            ctx.shadowOffsetX = 1;
            ctx.shadowOffsetY = 1;

            ctx.drawImage(logoImage, pos.x, pos.y, scaledWidth, scaledHeight);

            // Reset shadow
            ctx.shadowColor = "transparent";
            ctx.shadowBlur = 0;
            ctx.shadowOffsetX = 0;
            ctx.shadowOffsetY = 0;
        }

        animationFrame = requestAnimationFrame(draw);
    }

    function setupTamperDetection() {
        if (!canvasEl) return;
        const canvas = canvasEl;

        observer = new MutationObserver((mutations) => {
            for (const mutation of mutations) {
                if (mutation.type === "attributes") {
                    const attr = mutation.attributeName;
                    if (attr === "style" || attr === "class") {
                        // Check for visibility/opacity tampering
                        const style = window.getComputedStyle(canvas);
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
                    if (!document.contains(canvas)) {
                        handleTampering();
                    }
                }
            }
        });

        // Watch the canvas and its parent
        observer.observe(canvas.parentElement!, {
            childList: true,
            subtree: true,
            attributes: true,
            attributeFilter: ["style", "class"],
        });

        observer.observe(canvas, {
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

    onMount(async () => {
        if (mode !== "none") {
            // Load logo if needed
            if ((mode === "logo" || mode === "both") && logoUrl) {
                try {
                    logoImage = await loadLogo(logoUrl);
                    logoLoaded = true;
                    console.log("Logo loaded successfully:", logoUrl);
                } catch (err) {
                    console.error("Failed to load logo:", err);
                    // Continue without logo
                }
            }

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
