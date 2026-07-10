import { describe, it, expect, beforeEach, vi } from "vitest";
import { bindStream } from "./streamBinding";

// jsdom has no MediaStream/MediaStreamTrack — minimal fakes with the event
// surface the binding uses.
class FakeTrack extends EventTarget {
    kind = "video";
}

class FakeStream extends EventTarget {
    tracks: FakeTrack[];
    constructor(tracks: FakeTrack[]) {
        super();
        this.tracks = tracks;
    }
    getVideoTracks() {
        return this.tracks;
    }
    addTrack(track: FakeTrack) {
        this.tracks.push(track);
        this.dispatchEvent(new Event("addtrack"));
    }
}

function makeVideo() {
    const video = document.createElement("video");
    const play = vi.fn(() => Promise.resolve());
    Object.defineProperty(video, "play", { value: play });
    // jsdom throws assigning non-MediaStream to srcObject; shadow it.
    let src: unknown = null;
    Object.defineProperty(video, "srcObject", {
        get: () => src,
        set: (v) => {
            src = v;
        },
    });
    return { video, play };
}

describe("bindStream", () => {
    let video: HTMLVideoElement;
    let play: ReturnType<typeof vi.fn>;

    beforeEach(() => {
        ({ video, play } = makeVideo() as unknown as {
            video: HTMLVideoElement;
            play: ReturnType<typeof vi.fn>;
        });
    });

    it("attaches the stream and kicks playback", () => {
        const stream = new FakeStream([new FakeTrack()]) as unknown as MediaStream;
        bindStream(video, stream);
        expect(video.srcObject).toBe(stream);
        expect(play).toHaveBeenCalledTimes(1);
    });

    it("retries playback on loadeddata and track unmute", () => {
        const track = new FakeTrack();
        const stream = new FakeStream([track]) as unknown as MediaStream;
        bindStream(video, stream);
        play.mockClear();

        video.dispatchEvent(new Event("loadeddata"));
        expect(play).toHaveBeenCalledTimes(1);

        track.dispatchEvent(new Event("unmute"));
        expect(play).toHaveBeenCalledTimes(2);
    });

    it("rebinds and re-kicks when a track is swapped INTO the same stream", () => {
        // Device switch: new track, same MediaStream identity — `update`
        // never fires, so 'addtrack' must rewire the unmute listeners.
        const oldTrack = new FakeTrack();
        const fake = new FakeStream([oldTrack]);
        bindStream(video, fake as unknown as MediaStream);
        play.mockClear();

        const newTrack = new FakeTrack();
        fake.tracks = [newTrack];
        fake.dispatchEvent(new Event("addtrack"));
        expect(play).toHaveBeenCalledTimes(1);

        // Old track's listener must be gone; new track's must be live.
        play.mockClear();
        oldTrack.dispatchEvent(new Event("unmute"));
        expect(play).not.toHaveBeenCalled();
        newTrack.dispatchEvent(new Event("unmute"));
        expect(play).toHaveBeenCalledTimes(1);
    });

    it("update with the same stream is a no-op; a new stream rewires", () => {
        const a = new FakeStream([new FakeTrack()]);
        const action = bindStream(video, a as unknown as MediaStream);
        play.mockClear();

        action.update(a as unknown as MediaStream);
        expect(play).not.toHaveBeenCalled();

        const b = new FakeStream([new FakeTrack()]);
        action.update(b as unknown as MediaStream);
        expect(video.srcObject).toBe(b);
        expect(play).toHaveBeenCalledTimes(1);

        // a's addtrack must no longer re-kick
        play.mockClear();
        a.dispatchEvent(new Event("addtrack"));
        expect(play).not.toHaveBeenCalled();
    });

    it("update(null) detaches", () => {
        const stream = new FakeStream([new FakeTrack()]);
        const action = bindStream(video, stream as unknown as MediaStream);
        action.update(null);
        expect(video.srcObject).toBeNull();
    });

    it("destroy removes every listener and clears srcObject", () => {
        const track = new FakeTrack();
        const stream = new FakeStream([track]);
        const action = bindStream(video, stream as unknown as MediaStream);
        action.destroy();
        play.mockClear();

        expect(video.srcObject).toBeNull();
        video.dispatchEvent(new Event("loadeddata"));
        track.dispatchEvent(new Event("unmute"));
        stream.dispatchEvent(new Event("addtrack"));
        expect(play).not.toHaveBeenCalled();
    });
});
