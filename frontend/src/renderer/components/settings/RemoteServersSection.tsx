import { Server } from "lucide-react";
import { useCallback, useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import type {
	PublicRemoteConnectionStore,
	RemoteServerProfilePublic,
} from "../../../shared/remote-connection";
import { aoBridge } from "../../lib/bridge";
import { refreshDaemonStatus } from "../../lib/daemon-status";
import { ConfirmDialog } from "../ConfirmDialog";
import { Button } from "../ui/button";
import { Input } from "../ui/input";
import { SettingsSection } from "./SettingsSection";

function messageOf(error: unknown): string {
	return error instanceof Error ? error.message : String(error);
}

const EMPTY_FORM = { label: "", baseUrl: "", pinnedHostId: "", password: "" };

/**
 * Endpoint selection for the desktop: enroll a headless AO server, switch
 * between the local daemon and an enrolled server, and forget one.
 *
 * The connection password is typed once here and handed to the main process,
 * which stores it and owns every authenticated call. It is cleared from this
 * form immediately and never returned by the bridge.
 */
export function RemoteServersSection({ titleHidden }: { titleHidden?: boolean }) {
	const { t } = useTranslation();
	const [store, setStore] = useState<PublicRemoteConnectionStore | null>(null);
	const [loadError, setLoadError] = useState<string | null>(null);
	const [form, setForm] = useState(EMPTY_FORM);
	const [busy, setBusy] = useState(false);
	const [error, setError] = useState<string | null>(null);
	const [removing, setRemoving] = useState<RemoteServerProfilePublic | null>(null);

	const load = useCallback(async () => {
		try {
			const next = await aoBridge.remoteConnection.get();
			setStore(next);
			setLoadError(null);
		} catch (loadFailure) {
			setLoadError(messageOf(loadFailure));
		}
	}, []);

	useEffect(() => {
		void load();
	}, [load]);

	const apply = useCallback(async (next: PublicRemoteConnectionStore) => {
		setStore(next);
		// Main re-targets the daemon status; the renderer rebinds its transports
		// from that handshake.
		await refreshDaemonStatus();
	}, []);

	const run = useCallback(
		async (operation: () => Promise<PublicRemoteConnectionStore>) => {
			setBusy(true);
			setError(null);
			try {
				await apply(await operation());
				return true;
			} catch (operationFailure) {
				setError(messageOf(operationFailure));
				return false;
			} finally {
				setBusy(false);
			}
		},
		[apply],
	);

	const enroll = async () => {
		if (
			form.label.trim() === "" ||
			form.baseUrl.trim() === "" ||
			form.pinnedHostId.trim() === "" ||
			form.password === ""
		) {
			setError(t("settings.remote.fieldsRequired"));
			return;
		}
		const succeeded = await run(() =>
			aoBridge.remoteConnection.enroll({
				label: form.label.trim(),
				baseUrl: form.baseUrl.trim(),
				pinnedHostId: form.pinnedHostId.trim(),
				password: form.password,
			}),
		);
		// Never keep the secret in renderer state, even on failure.
		setForm(EMPTY_FORM);
		return succeeded;
	};

	const activeProfileId = store?.active.kind === "remote" ? store.active.profileId : null;

	return (
		<SettingsSection
			titleHidden={titleHidden}
			title={t("settings.remote")}
			sectionId="remote"
		>
			<div className="flex w-full flex-col gap-3 rounded-md bg-[var(--color-bg-settings-row)] p-3">
				<div className="flex items-start gap-2">
					<Server className="mt-0.5 size-icon-lg shrink-0 text-settings-muted" aria-hidden="true" />
					<div className="min-w-0">
						<p className="text-sm leading-5 text-settings-label">{t("settings.remote.title")}</p>
						<p className="mt-0.5 text-xs leading-4 text-settings-muted">
							{t("settings.remote.description")}
						</p>
					</div>
				</div>

				{loadError !== null ? (
					<p className="text-xs text-destructive">
						{t("settings.remote.loadFailed", { message: loadError })}
					</p>
				) : null}

				{store !== null ? (
					<div className="flex flex-col gap-1.5">
						<div className="flex items-center justify-between gap-2 rounded-sm border border-border/60 px-3 py-2">
							<span className="truncate text-sm text-settings-label">
								{t("settings.remote.localRow")}
							</span>
							{activeProfileId === null ? (
								<span className="text-xs text-settings-muted">{t("settings.remote.active")}</span>
							) : (
								<Button
									variant="outline"
									size="sm"
									disabled={busy}
									onClick={() => void run(() => aoBridge.remoteConnection.setActive({ kind: "local" }))}
								>
									{t("settings.remote.use")}
								</Button>
							)}
						</div>
						{store.profiles.map((profile) => {
							const active = profile.id === activeProfileId;
							return (
								<div
									key={profile.id}
									className="flex items-center justify-between gap-2 rounded-sm border border-border/60 px-3 py-2"
								>
									<span className="min-w-0">
										<span className="block truncate text-sm text-settings-label">{profile.label}</span>
										<span className="block truncate text-xs text-settings-muted">{profile.baseUrl}</span>
									</span>
									<span className="flex shrink-0 items-center gap-2">
										{active ? (
											<span className="text-xs text-settings-muted">{t("settings.remote.active")}</span>
										) : (
											<Button
												variant="outline"
												size="sm"
												disabled={busy}
												onClick={() =>
													void run(() =>
														aoBridge.remoteConnection.setActive({
															kind: "remote",
															profileId: profile.id,
														}),
													)
												}
											>
												{t("settings.remote.use")}
											</Button>
										)}
										<Button
											variant="outline"
											size="sm"
											disabled={busy}
											onClick={() => setRemoving(profile)}
										>
											{t("settings.remote.remove")}
										</Button>
									</span>
								</div>
							);
						})}
					</div>
				) : null}

				<div className="flex flex-col gap-2 border-t border-border/60 pt-3">
					<p className="text-sm leading-5 text-settings-label">{t("settings.remote.addTitle")}</p>
					<Input
						aria-label={t("settings.remote.label")}
						placeholder={t("settings.remote.labelPlaceholder")}
						value={form.label}
						onChange={(event) => setForm((current) => ({ ...current, label: event.target.value }))}
					/>
					<Input
						aria-label={t("settings.remote.address")}
						placeholder={t("settings.remote.addressPlaceholder")}
						value={form.baseUrl}
						onChange={(event) => setForm((current) => ({ ...current, baseUrl: event.target.value }))}
					/>
					<Input
						aria-label={t("settings.remote.hostId")}
						placeholder={t("settings.remote.hostIdPlaceholder")}
						value={form.pinnedHostId}
						onChange={(event) =>
							setForm((current) => ({ ...current, pinnedHostId: event.target.value }))
						}
					/>
					<Input
						type="password"
						aria-label={t("settings.remote.password")}
						placeholder={t("settings.remote.passwordPlaceholder")}
						value={form.password}
						onChange={(event) => setForm((current) => ({ ...current, password: event.target.value }))}
					/>
					<Button
						className="self-start"
						disabled={busy}
						onClick={() => void enroll()}
					>
						{busy ? t("settings.remote.adding") : t("settings.remote.add")}
					</Button>
				</div>

				{error !== null ? <p className="text-xs text-destructive">{error}</p> : null}
			</div>

			<ConfirmDialog
				open={removing !== null}
				title={t("settings.remote.removeTitle")}
				description={t("settings.remote.removeBody", { label: removing?.label ?? "" })}
				confirmLabel={t("settings.remote.remove")}
				destructive
				busy={busy}
				onOpenChange={(open) => {
					if (!open) setRemoving(null);
				}}
				onConfirm={() => {
					const profile = removing;
					if (profile === null) return;
					void run(() => aoBridge.remoteConnection.remove(profile.id)).then(() => setRemoving(null));
				}}
			/>
		</SettingsSection>
	);
}
