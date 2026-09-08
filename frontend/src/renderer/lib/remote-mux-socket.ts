// Renderer-side WebSocket stand-in that talks to main's remoteMux bridge.
// Pass as WebSocketImpl to createTerminalMux when remote mode is active.

import type { RemoteMuxClientEvent } from "../../main/remote-mux-bridge";
import { aoBridge } from "./bridge";

export type RemoteMuxBridgeApi = {
	connect: () => Promise<{ connectionId: string }>;
	subscribe: (connectionId: string) => void;
	send: (connectionId: string, data: string) => void;
	close: (connectionId: string) => void;
	onEvent: (connectionId: string, listener: (event: RemoteMuxClientEvent) => void) => () => void;
};

type Listener = (event: Event) => void;

/**
 * Minimal WebSocket-compatible client backed by Electron IPC. Only the surface
 * createTerminalMux uses is implemented (text frames, open/close/error/message).
 */
export function createRemoteMuxWebSocketClass(
	bridge: RemoteMuxBridgeApi = aoBridge.remoteMux,
): typeof WebSocket {
	return class RemoteMuxWebSocket extends EventTarget {
		static readonly CONNECTING = 0;
		static readonly OPEN = 1;
		static readonly CLOSING = 2;
		static readonly CLOSED = 3;

		readonly CONNECTING = 0;
		readonly OPEN = 1;
		readonly CLOSING = 2;
		readonly CLOSED = 3;

		readyState = RemoteMuxWebSocket.CONNECTING;
		bufferedAmount = 0;
		extensions = "";
		protocol = "";
		url: string;
		binaryType: BinaryType = "blob";

		onopen: ((this: WebSocket, ev: Event) => unknown) | null = null;
		onerror: ((this: WebSocket, ev: Event) => unknown) | null = null;
		onclose: ((this: WebSocket, ev: CloseEvent) => unknown) | null = null;
		onmessage: ((this: WebSocket, ev: MessageEvent) => unknown) | null = null;

		private connectionId: string | null = null;
		private unsubscribe: (() => void) | null = null;
		private readonly outboundQueue: string[] = [];

		constructor(url: string) {
			super();
			this.url = url;
			void this.start();
		}

		private async start(): Promise<void> {
			try {
				const { connectionId } = await bridge.connect();
				if (this.readyState === RemoteMuxWebSocket.CLOSED) {
					bridge.close(connectionId);
					return;
				}
				this.connectionId = connectionId;
				this.unsubscribe = bridge.onEvent(connectionId, (event) => this.onBridgeEvent(event));
				// Subscribe after the listener is attached so deferred open/message
				// events from a fast remote socket are replayed instead of dropped.
				bridge.subscribe(connectionId);
				for (const frame of this.outboundQueue.splice(0)) {
					bridge.send(connectionId, frame);
				}
			} catch (error) {
				this.dispatchError(error instanceof Error ? error.message : String(error));
				this.readyState = RemoteMuxWebSocket.CLOSED;
				this.dispatchEvent(new CloseEvent("close"));
				this.onclose?.(new CloseEvent("close"));
			}
		}

		private onBridgeEvent(event: RemoteMuxClientEvent): void {
			if (event.type === "open") {
				this.readyState = RemoteMuxWebSocket.OPEN;
				const ev = new Event("open");
				this.dispatchEvent(ev);
				this.onopen?.(ev);
				return;
			}
			if (event.type === "message") {
				const ev = new MessageEvent("message", { data: event.data });
				this.dispatchEvent(ev);
				this.onmessage?.(ev);
				return;
			}
			if (event.type === "error") {
				this.dispatchError(event.message);
				return;
			}
			if (event.type === "close") {
				this.readyState = RemoteMuxWebSocket.CLOSED;
				this.cleanup();
				const ev = new CloseEvent("close", {
					code: event.code ?? 1000,
					reason: event.reason ?? "",
				});
				this.dispatchEvent(ev);
				this.onclose?.(ev);
			}
		}

		private dispatchError(message: string): void {
			const ev = new Event("error");
			Object.defineProperty(ev, "message", { value: message });
			this.dispatchEvent(ev);
			this.onerror?.(ev);
		}

		private cleanup(): void {
			this.unsubscribe?.();
			this.unsubscribe = null;
		}

		send(data: string | ArrayBufferLike | Blob | ArrayBufferView): void {
			if (typeof data !== "string") {
				throw new TypeError("Remote mux only supports text frames.");
			}
			if (this.connectionId !== null && this.readyState === RemoteMuxWebSocket.OPEN) {
				bridge.send(this.connectionId, data);
				return;
			}
			this.outboundQueue.push(data);
		}

		close(): void {
			if (this.readyState === RemoteMuxWebSocket.CLOSED) return;
			this.readyState = RemoteMuxWebSocket.CLOSING;
			if (this.connectionId !== null) bridge.close(this.connectionId);
			this.cleanup();
			this.readyState = RemoteMuxWebSocket.CLOSED;
			const ev = new CloseEvent("close");
			this.dispatchEvent(ev);
			this.onclose?.(ev);
		}

		// EventTarget already implements addEventListener/removeEventListener;
		// createTerminalMux uses those exclusively.
		addEventListener(
			type: string,
			listener: EventListenerOrEventListenerObject | null,
			options?: boolean | AddEventListenerOptions,
		): void {
			super.addEventListener(type, listener as Listener, options);
		}
	} as unknown as typeof WebSocket;
}
