<script lang="ts">
    import { onMount } from "svelte";
    import { goto } from "$app/navigation";
    import { auth, rooms } from "$lib/api/client";

    onMount(async () => {
        // Check if authenticated by making a test API call
        // If not authenticated, the httpOnly cookie will be missing and we'll get 401
        try {
            await rooms.list();
        } catch {
            goto("/");
        }
    });

    let { children } = $props();

    async function logout() {
        try {
            await auth.logout();
        } finally {
            goto("/");
        }
    }
</script>

<div class="admin-layout">
    <aside class="sidebar">
        <div class="sidebar-header">
            <h1>Chromatic</h1>
        </div>

        <nav class="sidebar-nav">
            <a href="/admin" class="nav-item">Dashboard</a>
            <a href="/admin/setup" class="nav-item">Setup Wizard</a>
            <a href="/admin/rooms" class="nav-item">Rooms</a>
            <a href="/admin/stream-keys" class="nav-item">Stream Keys</a>
            <a href="/admin/settings" class="nav-item">Settings</a>
        </nav>

        <div class="sidebar-footer">
            <button class="btn btn-ghost" onclick={logout}> Logout </button>
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
    }

    .sidebar-header {
        padding: var(--space-lg);
        border-bottom: 1px solid var(--color-border);
    }

    .sidebar-header h1 {
        font-size: 1.25rem;
        background: linear-gradient(135deg, #3b82f6, #8b5cf6);
        -webkit-background-clip: text;
        -webkit-text-fill-color: transparent;
        background-clip: text;
    }

    .sidebar-nav {
        flex: 1;
        padding: var(--space-md);
    }

    .nav-item {
        display: block;
        padding: var(--space-sm) var(--space-md);
        margin-bottom: var(--space-xs);
        border-radius: var(--radius-md);
        color: var(--color-text-muted);
        transition: all var(--transition-fast);
    }

    .nav-item:hover {
        background: var(--color-surface-elevated);
        color: var(--color-text);
    }

    .sidebar-footer {
        padding: var(--space-md);
        border-top: 1px solid var(--color-border);
    }

    .sidebar-footer .btn {
        width: 100%;
        justify-content: flex-start;
    }

    .admin-main {
        flex: 1;
        padding: var(--space-xl);
        overflow-y: auto;
    }

    @media (max-width: 768px) {
        .sidebar {
            width: 60px;
        }

        .sidebar-header h1,
        .nav-item,
        .sidebar-footer {
            display: none;
        }
    }
</style>
