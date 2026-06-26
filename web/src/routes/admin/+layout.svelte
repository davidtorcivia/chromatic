<script lang="ts">
    import { onDestroy, onMount } from "svelte";
    import { goto } from "$app/navigation";
    import { page } from "$app/stores";
    import { auth, rooms } from "$lib/api/client";

    let destroyed = false;

    onDestroy(() => {
        destroyed = true;
    });

    onMount(async () => {
        // Check if authenticated by making a test API call
        // If not authenticated, the httpOnly cookie will be missing and we'll get 401
        try {
            await rooms.list();
        } catch {
            if (!destroyed) {
                goto("/");
            }
        }
    });

    let { children } = $props();

    const navItems = [
        {
            href: "/admin",
            label: "Dashboard",
            exact: true,
            icon: "M3 13h8V3H3v10zm0 8h8v-6H3v6zm10 0h8V11h-8v10zm0-18v6h8V3h-8z",
        },
        {
            href: "/admin/setup",
            label: "Setup Wizard",
            exact: false,
            icon: "M9 16.17L4.83 12l-1.42 1.41L9 19 21 7l-1.41-1.41L9 16.17z",
        },
        {
            href: "/admin/rooms",
            label: "Rooms",
            exact: false,
            icon: "M21 3H3c-1.1 0-2 .9-2 2v12c0 1.1.9 2 2 2h5v2h8v-2h5c1.1 0 2-.9 2-2V5c0-1.1-.9-2-2-2zm0 14H3V5h18v12z",
        },
        {
            href: "/admin/stream-keys",
            label: "Stream Keys",
            exact: false,
            icon: "M12.65 10C11.83 7.67 9.61 6 7 6c-3.31 0-6 2.69-6 6s2.69 6 6 6c2.61 0 4.83-1.67 5.65-4H17v4h4v-4h2v-4H12.65zM7 14c-1.1 0-2-.9-2-2s.9-2 2-2 2 .9 2 2-.9 2-2 2z",
        },
        {
            href: "/admin/settings",
            label: "Settings",
            exact: false,
            icon: "M19.14 12.94c.04-.3.06-.61.06-.94 0-.32-.02-.64-.07-.94l2.03-1.58c.18-.14.23-.41.12-.61l-1.92-3.32c-.12-.22-.37-.29-.59-.22l-2.39.96c-.5-.38-1.03-.7-1.62-.94l-.36-2.54c-.04-.24-.24-.41-.48-.41h-3.84c-.24 0-.43.17-.47.41l-.36 2.54c-.59.24-1.13.57-1.62.94l-2.39-.96c-.22-.08-.47 0-.59.22L2.74 8.87c-.12.21-.08.47.12.61l2.03 1.58c-.05.3-.09.63-.09.94s.02.64.07.94l-2.03 1.58c-.18.14-.23.41-.12.61l1.92 3.32c.12.22.37.29.59.22l2.39-.96c.5.38 1.03.7 1.62.94l.36 2.54c.05.24.24.41.48.41h3.84c.24 0 .44-.17.47-.41l.36-2.54c.59-.24 1.13-.56 1.62-.94l2.39.96c.22.08.47 0 .59-.22l1.92-3.32c.12-.22.07-.47-.12-.61l-2.01-1.58zM12 15.6c-1.98 0-3.6-1.62-3.6-3.6s1.62-3.6 3.6-3.6 3.6 1.62 3.6 3.6-1.62 3.6-3.6 3.6z",
        },
    ];

    function isActive(item: { href: string; exact: boolean }, pathname: string) {
        return item.exact
            ? pathname === item.href
            : pathname === item.href || pathname.startsWith(item.href + "/");
    }

    async function logout() {
        try {
            await auth.logout();
        } finally {
            if (!destroyed) {
                goto("/");
            }
        }
    }
</script>

<div class="admin-layout">
    <aside class="sidebar">
        <div class="sidebar-header">
            <a href="/admin" class="brand">
                <span class="brand-dot" aria-hidden="true"></span>
                <span class="brand-name">Chromatic</span>
            </a>
        </div>

        <nav class="sidebar-nav">
            {#each navItems as item (item.href)}
                <a
                    href={item.href}
                    class="nav-item"
                    class:active={isActive(item, $page.url.pathname)}
                    aria-current={isActive(item, $page.url.pathname)
                        ? "page"
                        : undefined}
                    title={item.label}
                >
                    <svg
                        class="nav-icon"
                        viewBox="0 0 24 24"
                        fill="currentColor"
                        aria-hidden="true"
                    >
                        <path d={item.icon} />
                    </svg>
                    <span class="nav-label">{item.label}</span>
                </a>
            {/each}
        </nav>

        <div class="sidebar-footer">
            <button class="btn btn-ghost" onclick={logout} title="Logout">
                <svg
                    class="nav-icon"
                    viewBox="0 0 24 24"
                    fill="currentColor"
                    aria-hidden="true"
                >
                    <path
                        d="M17 7l-1.41 1.41L18.17 11H8v2h10.17l-2.58 2.58L17 17l5-5zM4 5h8V3H4c-1.1 0-2 .9-2 2v14c0 1.1.9 2 2 2h8v-2H4V5z"
                    />
                </svg>
                <span class="nav-label">Logout</span>
            </button>
        </div>
    </aside>

    <main class="admin-main">
        {@render children()}
    </main>
</div>

<style>
    .admin-layout {
        display: flex;
        min-height: 100vh;
    }

    .sidebar {
        width: 240px;
        background: var(--color-surface);
        border-right: 1px solid var(--color-border);
        display: flex;
        flex-direction: column;
        flex-shrink: 0;
    }

    .sidebar-header {
        padding: var(--space-lg);
        border-bottom: 1px solid var(--color-border);
    }

    .brand {
        display: flex;
        align-items: center;
        gap: var(--space-sm);
        color: var(--color-text);
    }

    .brand:hover {
        color: var(--color-text);
    }

    .brand-dot {
        width: 10px;
        height: 10px;
        border-radius: 50%;
        background: var(--color-primary);
        flex-shrink: 0;
    }

    .brand-name {
        font-family: var(--font-display);
        font-size: 1.125rem;
        font-weight: 600;
        letter-spacing: -0.01em;
    }

    .sidebar-nav {
        flex: 1;
        padding: var(--space-md);
    }

    .nav-item {
        display: flex;
        align-items: center;
        gap: var(--space-sm);
        padding: var(--space-sm) var(--space-md);
        margin-bottom: var(--space-xs);
        border-radius: var(--radius-md);
        border-left: 2px solid transparent;
        color: var(--color-text-muted);
        font-size: 0.875rem;
        transition: all var(--transition-fast);
    }

    .nav-item:hover {
        background: var(--color-surface-elevated);
        color: var(--color-text);
    }

    .nav-item.active {
        background: var(--color-surface-elevated);
        color: var(--color-text);
        border-left-color: var(--color-primary);
    }

    .nav-icon {
        width: 18px;
        height: 18px;
        flex-shrink: 0;
        opacity: 0.8;
    }

    .sidebar-footer {
        padding: var(--space-md);
        border-top: 1px solid var(--color-border);
    }

    .sidebar-footer .btn {
        width: 100%;
        justify-content: flex-start;
        gap: var(--space-sm);
    }

    .admin-main {
        flex: 1;
        padding: var(--space-xl);
        overflow-y: auto;
        min-width: 0;
    }

    @media (max-width: 768px) {
        .sidebar {
            width: 64px;
        }

        .brand-name,
        .nav-label {
            display: none;
        }

        .sidebar-header {
            display: flex;
            justify-content: center;
            padding: var(--space-md);
        }

        .sidebar-nav {
            padding: var(--space-sm);
        }

        .nav-item {
            justify-content: center;
            padding: var(--space-sm);
        }

        .sidebar-footer {
            padding: var(--space-sm);
        }

        .sidebar-footer .btn {
            justify-content: center;
        }

        .admin-main {
            padding: var(--space-md);
        }
    }
</style>
