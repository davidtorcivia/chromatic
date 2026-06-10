<script lang="ts">
    import { onMount, onDestroy } from "svelte";

    interface WatermarkBounds {
        x: number;
        y: number;
        width: number;
        height: number;
    }

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
        // Fractional center (0-1) of the watermark within the video.
        // null/undefined = legacy built-in placement (text bottom-right,
        // logo at logoPosition).
        posX?: number | null;
        posY?: number | null;
        // Size multiplier applied to the base font size / logo size.
        scale?: number;
        // Reports the pixel bounds of the drawn watermark after each draw
        // (null when nothing was drawn). Used by the admin preview for
        // drag-to-position hit testing and the outline.
        onBoundsChange?: (bounds: WatermarkBounds | null) => void;
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
        posX = null,
        posY = null,
        scale = 1,
        onBoundsChange,
    }: Props = $props();

    let canvasEl = $state<HTMLCanvasElement | null>(null);
    let observer: MutationObserver;
    let resizeObserver: ResizeObserver | null = null;
    let redrawInterval: ReturnType<typeof setInterval> | null = null;
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

    // Calculate logo corner position based on logoPosition prop (legacy
    // placement, used when no custom posX/posY is set)
    function getLogoCornerPosition(
        canvasWidth: number,
        canvasHeight: number,
        scaledWidth: number,
        scaledHeight: number,
    ): { x: number; y: number } {
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
        const w = parent.clientWidth;
        const h = parent.clientHeight;
        canvasEl.width = w;
        canvasEl.height = h;

        ctx.clearRect(0, 0, w, h);
        ctx.globalAlpha = opacity;

        // Effective size multiplier (defensive clamp, mirrors server limits)
        const effScale = Math.min(3, Math.max(0.25, scale ?? 1));
        const fontSize = 14 * effScale;
        const effLogoSize = logoSize * effScale;

        // Custom fractional center position (null = legacy placement)
        const hasCustomPos = posX != null && posY != null;
        const cx = hasCustomPos ? (posX as number) * w : 0;
        const cy = hasCustomPos ? (posY as number) * h : 0;

        // Union of drawn element bounds (reported via onBoundsChange)
        let bounds: WatermarkBounds | null = null;
        const addBounds = (x: number, y: number, bw: number, bh: number) => {
            if (!bounds) {
                bounds = { x, y, width: bw, height: bh };
                return;
            }
            const x2 = Math.max(bounds.x + bounds.width, x + bw);
            const y2 = Math.max(bounds.y + bounds.height, y + bh);
            bounds.x = Math.min(bounds.x, x);
            bounds.y = Math.min(bounds.y, y);
            bounds.width = x2 - bounds.x;
            bounds.height = y2 - bounds.y;
        };

        // Logo metrics (needed before drawing text so "both" mode can stack
        // the text below the logo at a custom position)
        const drawLogo = (mode === "logo" || mode === "both") && logoImage && logoLoaded;
        let scaledLogoWidth = 0;
        let scaledLogoHeight = 0;
        if (drawLogo && logoImage) {
            const fit = Math.min(
                effLogoSize / logoImage.width,
                effLogoSize / logoImage.height,
            );
            scaledLogoWidth = logoImage.width * fit;
            scaledLogoHeight = logoImage.height * fit;
        }

        // Draw logo watermark
        if (drawLogo && logoImage) {
            const pos = hasCustomPos
                ? { x: cx - scaledLogoWidth / 2, y: cy - scaledLogoHeight / 2 }
                : getLogoCornerPosition(w, h, scaledLogoWidth, scaledLogoHeight);

            // Add subtle shadow for visibility on any background
            ctx.shadowColor = "rgba(0, 0, 0, 0.5)";
            ctx.shadowBlur = 4;
            ctx.shadowOffsetX = 1;
            ctx.shadowOffsetY = 1;

            ctx.drawImage(logoImage, pos.x, pos.y, scaledLogoWidth, scaledLogoHeight);
            addBounds(pos.x, pos.y, scaledLogoWidth, scaledLogoHeight);

            // Reset shadow
            ctx.shadowColor = "transparent";
            ctx.shadowBlur = 0;
            ctx.shadowOffsetX = 0;
            ctx.shadowOffsetY = 0;
        }

        // Draw text watermark (legacy: bottom-right corner)
        if ((mode === "text" || mode === "both") && text) {
            const processedText = processText(text);
            ctx.font = `${fontSize}px 'Work Sans', sans-serif`;
            ctx.fillStyle = "white";
            ctx.shadowColor = "rgba(0, 0, 0, 0.8)";
            ctx.shadowBlur = 2;

            const textWidth = ctx.measureText(processedText).width;
            const textHeight = fontSize * 1.2;

            if (hasCustomPos) {
                ctx.textAlign = "center";
                ctx.textBaseline = "middle";
                // In "both" mode the text sits just below the logo so the
                // pair moves as one unit around the shared center.
                const textCy = drawLogo
                    ? cy + scaledLogoHeight / 2 + textHeight / 2 + 4 * effScale
                    : cy;
                ctx.fillText(processedText, cx, textCy);
                addBounds(cx - textWidth / 2, textCy - textHeight / 2, textWidth, textHeight);
            } else {
                ctx.textAlign = "right";
                ctx.textBaseline = "bottom";
                ctx.fillText(processedText, w - logoPadding, h - logoPadding);
                addBounds(
                    w - logoPadding - textWidth,
                    h - logoPadding - textHeight,
                    textWidth,
                    textHeight,
                );
            }

            ctx.shadowColor = "transparent";
            ctx.shadowBlur = 0;
        }

        onBoundsChange?.(bounds);
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
        const parent = canvas.parentElement;
        if (parent) {
            observer.observe(parent, {
                childList: true,
                subtree: true,
                attributes: true,
                attributeFilter: ["style", "class"],
            });
        }

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

            // The watermark is static between size changes and timestamp
            // updates, so draw on demand only (not in a RAF loop):
            // once now, on container resize, and every 60s for {{ time }}.
            draw();

            const parent = canvasEl?.parentElement;
            if (parent && typeof ResizeObserver !== "undefined") {
                resizeObserver = new ResizeObserver(() => draw());
                resizeObserver.observe(parent);
            }

            redrawInterval = setInterval(() => draw(), 60_000);

            setupTamperDetection();
        }
    });

    // Redraw when watermark configuration changes (draw() reads the props,
    // so the effect tracks them automatically), or when the canvas binds.
    $effect(() => {
        draw();
    });

    onDestroy(() => {
        if (resizeObserver) {
            resizeObserver.disconnect();
            resizeObserver = null;
        }
        if (redrawInterval) {
            clearInterval(redrawInterval);
            redrawInterval = null;
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
