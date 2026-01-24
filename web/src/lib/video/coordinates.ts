// Video coordinate utilities for letterbox-aware laser pointer

export interface VideoRect {
    x: number;      // Left edge of video content within element
    y: number;      // Top edge of video content within element
    width: number;  // Rendered width of video content
    height: number; // Rendered height of video content
}

/**
 * Get the actual video content rectangle within a video element
 * that uses object-fit: contain (which may have letterboxing)
 */
export function getVideoContentRect(video: HTMLVideoElement): VideoRect {
    const elementRect = video.getBoundingClientRect();
    const elementWidth = elementRect.width;
    const elementHeight = elementRect.height;

    const videoWidth = video.videoWidth;
    const videoHeight = video.videoHeight;

    if (!videoWidth || !videoHeight) {
        // Fallback if video dimensions not yet available
        return { x: 0, y: 0, width: elementWidth, height: elementHeight };
    }

    const elementAspect = elementWidth / elementHeight;
    const videoAspect = videoWidth / videoHeight;

    let renderedWidth: number;
    let renderedHeight: number;

    if (videoAspect > elementAspect) {
        // Video is wider than element: letterbox top/bottom
        renderedWidth = elementWidth;
        renderedHeight = elementWidth / videoAspect;
    } else {
        // Video is taller than element: pillarbox left/right
        renderedHeight = elementHeight;
        renderedWidth = elementHeight * videoAspect;
    }

    const x = (elementWidth - renderedWidth) / 2;
    const y = (elementHeight - renderedHeight) / 2;

    return { x, y, width: renderedWidth, height: renderedHeight };
}

/**
 * Convert client (mouse/touch) coordinates to normalized video coordinates (0-1)
 */
export function clientToVideoCoords(
    clientX: number,
    clientY: number,
    video: HTMLVideoElement
): { x: number; y: number; valid: boolean } {
    const elementRect = video.getBoundingClientRect();
    const videoRect = getVideoContentRect(video);

    // Convert client coords to element-relative
    const elementX = clientX - elementRect.left;
    const elementY = clientY - elementRect.top;

    // Convert to video-content-relative
    const videoX = elementX - videoRect.x;
    const videoY = elementY - videoRect.y;

    // Normalize to 0-1 range
    const normalizedX = videoX / videoRect.width;
    const normalizedY = videoY / videoRect.height;

    // Check if click was in letterbox/pillarbox area
    const valid = normalizedX >= 0 && normalizedX <= 1 &&
        normalizedY >= 0 && normalizedY <= 1;

    return {
        x: Math.max(0, Math.min(1, normalizedX)),
        y: Math.max(0, Math.min(1, normalizedY)),
        valid
    };
}

/**
 * Convert normalized video coordinates (0-1) to pixel position within video element
 */
export function videoToElementCoords(
    normX: number,
    normY: number,
    video: HTMLVideoElement
): { x: number; y: number } {
    const videoRect = getVideoContentRect(video);

    return {
        x: videoRect.x + normX * videoRect.width,
        y: videoRect.y + normY * videoRect.height
    };
}
