<script lang="ts">
    import { onMount } from "svelte";

    interface BrowserInfo {
        browser: string;
        os: string;
        isChrome: boolean;
        isSafari: boolean;
        isMac: boolean;
    }

    let isVisible = $state(false);
    let dismissed = $state(false);

    function detectBrowser(): BrowserInfo {
        const ua = navigator.userAgent;
        const isChrome = /Chrome/.test(ua) && !/Chromium|Edge/.test(ua);
        const isSafari = /Safari/.test(ua) && !/Chrome|Chromium/.test(ua);
        const isMac = /Mac OS X/.test(ua);

        let browser = "Unknown";
        if (isChrome) browser = "Chrome";
        else if (isSafari) browser = "Safari";
        else if (/Firefox/.test(ua)) browser = "Firefox";
        else if (/Edge/.test(ua)) browser = "Edge";

        let os = "Unknown";
        if (isMac) os = "macOS";
        else if (/Windows/.test(ua)) os = "Windows";
        else if (/Linux/.test(ua)) os = "Linux";
        else if (/Android/.test(ua)) os = "Android";
        else if (/iPhone|iPad/.test(ua)) os = "iOS";

        return { browser, os, isChrome, isSafari, isMac };
    }

    function dismiss() {
        dismissed = true;
        isVisible = false;
        // Remember dismissal for this session
        sessionStorage.setItem("chromatic_browser_toast_dismissed", "true");
    }

    onMount(() => {
        // Check if already dismissed this session
        if (sessionStorage.getItem("chromatic_browser_toast_dismissed")) {
            return;
        }

        const browserInfo = detectBrowser();

        // Show toast for Chrome on Mac (Safari has better color management)
        if (browserInfo.isChrome && browserInfo.isMac) {
            setTimeout(() => {
                isVisible = true;
            }, 2000); // Show after 2 seconds

            // Auto-hide after 10 seconds
            setTimeout(() => {
                if (!dismissed) {
                    isVisible = false;
                }
            }, 12000);
        }
    });
</script>

{#if isVisible && !dismissed}
    <div class="toast" role="alert">
        <div class="toast-content">
            <div class="toast-icon">
                <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
                    <circle cx="12" cy="12" r="10" />
                    <line x1="12" y1="8" x2="12" y2="12" />
                    <line x1="12" y1="16" x2="12.01" y2="16" />
                </svg>
            </div>
            <div class="toast-text">
                <strong>Color Accuracy Tip</strong>
                <p>For best color accuracy in color grading workflows, Safari on macOS provides better display color management.</p>
            </div>
            <button class="toast-close" onclick={dismiss} aria-label="Dismiss">
                <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
                    <line x1="18" y1="6" x2="6" y2="18" />
                    <line x1="6" y1="6" x2="18" y2="18" />
                </svg>
            </button>
        </div>
    </div>
{/if}

<style>
    .toast {
        position: fixed;
        bottom: 1.5rem;
        left: 50%;
        transform: translateX(-50%);
        z-index: 1000;
        animation: slideUp 0.3s ease-out;
    }

    @keyframes slideUp {
        from {
            opacity: 0;
            transform: translateX(-50%) translateY(1rem);
        }
        to {
            opacity: 1;
            transform: translateX(-50%) translateY(0);
        }
    }

    .toast-content {
        display: flex;
        align-items: flex-start;
        gap: 0.75rem;
        padding: 1rem 1.25rem;
        max-width: 400px;
        /* No background/border — floats over the dark UI; a soft text
           shadow keeps it readable over video. */
        text-shadow: 0 1px 3px rgba(0, 0, 0, 0.8);
    }

    .toast-icon {
        flex-shrink: 0;
        color: var(--color-warning, #f59e0b);
        margin-top: 0.125rem;
    }

    .toast-text {
        flex: 1;
    }

    .toast-text strong {
        display: block;
        font-size: 0.875rem;
        font-weight: 600;
        color: var(--color-text, #fff);
        margin-bottom: 0.25rem;
    }

    .toast-text p {
        font-size: 0.8125rem;
        color: var(--color-text-muted, #a0a0a0);
        margin: 0;
        line-height: 1.4;
    }

    .toast-close {
        flex-shrink: 0;
        background: none;
        border: none;
        color: var(--color-text-muted, #a0a0a0);
        cursor: pointer;
        padding: 0.25rem;
        border-radius: var(--radius-sm, 0.25rem);
        transition: color 0.15s, background 0.15s;
    }

    .toast-close:hover {
        color: var(--color-text, #fff);
        background: rgba(255, 255, 255, 0.1);
    }
</style>
