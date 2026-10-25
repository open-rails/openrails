import type { BillingUiMessageBundle } from "../i18n/messages"

export const de: BillingUiMessageBundle = {
  billingNotifications: {
    paymentMethodRequiredTitle: "Zahlungsmethode aktualisieren",
    authenticationRequiredTitle: "Zahlungsauthentifizierung erforderlich",
    collectionStoppedTitle: "Automatische Zahlungen gestoppt",

    paymentFailed: "Zahlung fehlgeschlagen",
    membershipEnded: "Premium-Mitgliedschaft beendet",
    membershipDowngraded: "Mitgliedschaft herabgestuft",
    retryScheduled:
      "Bei Erstellung dieser Mitteilung war ein weiterer Zahlungsversuch für {date} geplant.",
    paymentMethodRequired:
      "Vor einem weiteren Zahlungsversuch war eine neue Zahlungsmethode erforderlich.",
    retryStopped: "Es war kein weiterer automatischer Zahlungsversuch geplant.",
    retryUnknown:
      "Bei Erstellung dieser Mitteilung lag kein Wiederholungsplan vor.",
    endedAt: "Dein Premium-Zugang endete am {date}.",
    ended: "Dein Premium-Zugang ist beendet.",
    downgradedTo: "Deine Mitgliedschaft wurde von {from} auf {to} geändert.",
    downgraded: "Die geplante Herabstufung deiner Mitgliedschaft ist erfolgt.",
    instructions: {
      update_payment_method:
        "Aktualisiere deine Zahlungsmethode und prüfe den aktuellen Zahlungsstatus auf der Abrechnungsseite.",
      resubscribe:
        "Wähle eine Mitgliedschaft, um den Zugang wiederherzustellen. Prüfe vor dem Abschluss die Abrechnungsseite.",
      authenticate_payment:
        "Schließe die Zahlungsauthentifizierung auf der Abrechnungsseite ab.",
      contact_support: "Wende dich wegen dieser Zahlung an den Support.",
      manage_billing:
        "Prüfe auf der Abrechnungsseite den aktuellen Zahlungsstatus und die nächsten Schritte.",
    },
    actions: {
      update_payment_method: "Zahlungsmethode aktualisieren",
      resubscribe: "Mitgliedschaft wählen",
      authenticate_payment: "Authentifizierung abschließen",
      contact_support: "Support kontaktieren",
      manage_billing: "Abrechnung ansehen",
    },
  },

  common: {
    cancel: "Abbrechen",
    close: "Schließen",
    retry: "Erneut versuchen",
    loading: "Wird geladen...",
    previous: "Zurück",
    next: "Weiter",
    notAvailable: "—",
  },
  status: {
    active: "Aktiv",
    pending: "Ausstehend",
    past_due: "Überfällig",
    canceled: "Gekündigt",
    cancel_scheduled: "Endet",
    unknown: "Unbekannt",
    awaiting_method: "Karte aktualisieren",
    unverified: "Wird geprüft",
    succeeded: "Bezahlt",
    failed: "Fehlgeschlagen",
    refunded: "Erstattet",
    partially_refunded: "Teilweise erstattet",
    expired: "Abgelaufen",
    expiring_soon: "Läuft bald ab",
    needs_attention: "Prüfen",
    default: "Standard",
  },
  interval: {
    every: {
      hour: { one: "stündlich", other: "alle {count} Stunden" },
      day: { one: "täglich", other: "alle {count} Tage" },
      week: { one: "wöchentlich", other: "alle {count} Wochen" },
    },
    per: {
      hour: { one: "/ Stunde", other: "/ {count} Stunden" },
      day: { one: "/ Tag", other: "/ {count} Tage" },
      week: { one: "/ Woche", other: "/ {count} Wochen" },
    },
    access: {
      hour: { one: "1 Stunde Zugang", other: "{count} Stunden Zugang" },
      day: { one: "1 Tag Zugang", other: "{count} Tage Zugang" },
      week: { one: "1 Woche Zugang", other: "{count} Wochen Zugang" },
    },
    duration: {
      hour: { one: "1 Stunde", other: "{count} Stunden" },
      day: { one: "1 Tag", other: "{count} Tage" },
      week: { one: "1 Woche", other: "{count} Wochen" },
    },
    once: "einmalig",
    permanent: "Zugriff ohne geplantes Ablaufdatum",
  },
  products: {
    empty: "Nichts im Angebot schaltet dies frei",
    once: "{amount}",
    rent: "{period} leihen, {amount}",
    recurring: "{amount} {every}",
  },
  subscriptions: {
    title: "Abonnements",
    description: "Deine Pläne und wann sie sich verlängern.",
    empty: "Noch keine Abonnements",
    emptyDescription: "Abgeschlossene Pläne erscheinen hier.",
    browsePlans: "Pläne ansehen",
    fallbackName: "Abonnement",
    renews: "Verlängert sich am {date}",
    endsOn: "Zugriff bis {date}",
    noMorePayments: "Keine weiteren Zahlungen",
    endedOn: "Beendet am {date}",
    pastDue:
      "Zahlung fehlgeschlagen. Aktualisiere deine Karte, um den Zugriff zu behalten.",
    cancel: "Kündigen",
    cancelLabel: "{name} kündigen",
    resume: "Fortsetzen",
    resumed: "Dein Abonnement läuft weiter.",
    manageAtProvider: "Verwalten",
    updating: "Wird aktualisiert...",
    paidWith: "{brand} •••• {last4}",
    wallet: "Solana-Wallet",
    changeCard: "Karte ändern",
    changeCardLabel: "Karte für {name} ändern",
    changePlan: "Plan ändern",
    changePlanLabel: "Plan für {name} ändern",
  },
  resume: {
    chooseCard: "Zahlungsmethode auswählen",
    savedCardUnavailable:
      "Diese gespeicherte Karte ist nicht verfügbar. Wähle eine andere Karte oder füge eine hinzu.",
    title: "{name} fortsetzen?",
    description:
      "Dein Abo verlängert sich am {date}. Dann werden {price} von deiner Karte abgebucht, danach in jedem Abrechnungszeitraum bis zur Kündigung.",
    descriptionNoDate:
      "Dein Abo verlängert sich, und deine Karte wird in jedem Abrechnungszeitraum belastet, bis du kündigst.",
    keepCanceled: "Gekündigt lassen",
    confirm: "Abo fortsetzen",
  },
  changeCard: {
    title: "{name} bezahlen mit",
    description:
      "Künftige Verlängerungen werden von der gewählten Karte abgebucht.",
    none: "Füge unter Zahlungsmethoden eine weitere Karte hinzu, um zu wechseln.",
    confirm: "Diese Karte verwenden",
    current: "Aktuell",
    useDefault: "Ihre Standardkarte ({card})",
    done: "Zahlungskarte aktualisiert.",
  },
  changePlan: {
    title: "{name} ändern",
    description:
      "Ein Upgrade gilt sofort, ein Downgrade ab deiner nächsten Verlängerung.",
    none: "Derzeit gibt es keine anderen Pläne zum Wechseln.",
    chargeNow: "{amount} jetzt, danach {next} ab {date}",
    chargeNowEstimate: "Etwa {amount} jetzt, danach {next} ab {date}",
    atPeriodEnd: "Jetzt keine Kosten. Wechsel am {date}, danach {next}",
    confirm: "Plan ändern",
    signing: "Warte auf die Wallet...",
    confirming: "Wird bestätigt...",
    done: "Du nutzt jetzt {name}.",
    scheduled: "{name} beginnt am {date}.",
    processing:
      "Tarifwechsel übermittelt; er wird wirksam, sobald dein Anbieter ihn bestätigt.",
    actionRequired:
      "Für diesen Wechsel ist ein weiterer Schritt nötig. Versuche es erneut.",
  },
  renewal: {
    title: "Abonnement erneuern",
    description:
      "Dein Abonnement ist abgelaufen. Erneuere deinen Plan, um wieder Zugang zu erhalten.",
    action: "Jetzt erneuern",
  },
  cancel: {
    title: "{name} kündigen?",
    description:
      "Künftige Zahlungen werden eingestellt. Bereits gekaufter Zugriff bleibt bestehen.",
    revokeOnChain:
      "Auch auf der Blockchain kündigen (erfordert Wallet-Signatur und Netzwerkgebühr).",
    solanaDescription:
      "Deine Wallet signiert eine On-Chain-Kündigung. Bereits gekaufter Zugriff bleibt bestehen.",
    reasonLabel: "Warum kündigst du?",
    reasonPlaceholder: "Dein Feedback hilft uns, besser zu werden.",
    reasonHint: "{count}/{max}",
    reasonTooShort: "Gib mindestens {min} Zeichen ein.",
    keep: "Abonnement behalten",
    confirm: "Abonnement kündigen",
    confirmWallet: "Mit Wallet signieren",
    working: "Wird gekündigt...",
    preparing: "Transaktion wird vorbereitet...",
    signing: "In deiner Wallet bestätigen...",
    confirming: "Kündigung wird erfasst...",
    done: "Kündigung angefordert.",
  },
  paymentMethods: {
    title: "Zahlungsmethoden",
    description: "Gespeicherte Karten für deine Abonnements.",
    empty: "Keine gespeicherten Karten",
    emptyDescription: "Gespeicherte Karten erscheinen hier.",
    add: "Karte hinzufügen",
    addTitle: "Karte hinzufügen",
    addDescription:
      "Kartendaten gehen direkt an den Zahlungsdienstleister; diese Seite sieht sie nie.",
    save: "Karte speichern",
    added: "Karte gespeichert.",
    provider: "Zahlungsanbieter",
    saveNotice:
      "Eingegebene Karten werden für künftige Zahlungen in Ihrem Konto gespeichert.",
    unavailable:
      "Sichere Karteneingabe ist für diesen Anbieter nicht verfügbar.",
    addUnavailable: "Karten können vorübergehend nicht hinzugefügt werden.",
    verificationPending:
      "Die Kartenprüfung läuft noch. Prüfe deine gespeicherten Karten, bevor du es erneut versuchst.",
    cardLabel: "{brand} •••• {last4}",
    fallbackBrand: "Karte",
    expires: "Gültig bis {date}",
    usedBy: "Verwendet von {names}",
    defaultFor: "Standard für {currencies}",
    makeDefault: "Als Standard festlegen",
    defaultUpdated: "Standardkarte aktualisiert.",
    remove: "Entfernen",
    removeLabel: "{label} entfernen",
    removeTitle: "{label} entfernen?",
    removeDescription:
      "Eine Karte, die ein aktives Abonnement bezahlt, muss vor dem Entfernen ersetzt werden.",
    removeConfirm: "Karte entfernen",
    removed: "Karte entfernt.",
    removeFailed:
      "Die Karte konnte nicht bestätigt entfernt werden. Bitte versuche es erneut.",
    inUse:
      "Diese Karte bezahlt ein aktives Abonnement. Stelle das Abonnement zuerst auf eine andere Karte um.",
  },
  purchase: {
    renews: "Verlängert sich {period}",
  },
  history: {
    title: "Zahlungsverlauf",
    description: "Belastungen und Erstattungen deines Kontos.",
    empty: "Noch keine Zahlungen",
    emptyDescription: "Abgeschlossene Zahlungen erscheinen hier.",
    date: "Datum",
    item: "Artikel",
    subscription: "Abonnement",
    purchase: "Kauf",
    method: "Methode",
    status: "Status",
    amount: "Betrag",
    refund: "Erstattung",
    refundedAmount: "{amount} erstattet",
    page: "Seite {page}",
    rail: {
      card: "Karte",
      solana: "Solana",
      ccbill: "Karte",
      nmi: "Karte",
      stripe: "Karte",
    },
  },
  errors: {
    generic: "Etwas ist schiefgelaufen. Bitte versuche es erneut.",
    network_error:
      "Der Server ist nicht erreichbar. Prüfe deine Verbindung und versuche es erneut.",
    server_unavailable:
      "Der Server antwortet gerade nicht. Bitte versuche es gleich noch einmal.",
    invalid_response: "Der Server hat eine unerwartete Antwort gesendet.",
    authentication_required:
      "Deine Sitzung ist abgelaufen. Melde dich erneut an.",
    resource_access_denied: "Du hast darauf keinen Zugriff.",
    resource_not_found: "Dieser Eintrag existiert nicht mehr.",
    resource_conflict:
      "Das hat sich inzwischen geändert. Lade neu und versuche es erneut.",
    rate_limit_exceeded:
      "Zu viele Anfragen. Warte einen Moment und versuche es erneut.",
    provider_writes_disabled:
      "Zahlungen sind auf dieser Seite gerade abgeschaltet.",
    service_unavailable:
      "Die Abrechnung ist vorübergehend nicht verfügbar. Versuche es bald erneut.",
    internal_error:
      "Bei uns ist etwas schiefgelaufen. Bitte versuche es erneut.",
    card_declined: "Die Karte wurde abgelehnt.",
    payment_provider_rejected:
      "Der Zahlungsdienstleister hat die Anfrage abgelehnt.",
    provider_outcome_unknown:
      "Wir konnten das Ergebnis beim Anbieter nicht bestätigen. Prüfe es in Kürze erneut.",
    payment_in_progress: "Eine Zahlung ist bereits in Bearbeitung.",
    payment_method_delete_unsupported:
      "Diese Karte kann hier nicht entfernt werden. Wende dich an den Support.",
    payment_method_delete_failed:
      "Der Anbieter konnte die Karte nicht entfernen. Versuche es später erneut.",
    customer_session_required: "Melde dich erneut an, um dies zu tun.",
    invoker_scoped_principal: "Dieses Konto kann keine Abrechnung verwalten.",
    csrf_origin_denied: "Die Anfrage wurde blockiert. Lade die Seite neu.",
    wallet_rejected: "Die Wallet-Anfrage wurde abgelehnt.",
    wallet_no_signature: "Die Wallet hat die Transaktion nicht signiert.",
    subscription_change_in_flight:
      "Ein Planwechsel für dieses Abonnement wird noch verarbeitet. Bitte warte, bis er abgeschlossen ist.",
    subscription_change_idempotency_conflict:
      "Dieser Planwechsel wurde bereits mit anderen Angaben übermittelt. Bitte versuche es erneut.",
    subscription_change_already_scheduled:
      "Für deine nächste Verlängerung ist bereits ein Planwechsel geplant.",
    subscription_change_target_inactive:
      "Dieser Plan ist nicht mehr verfügbar.",
    subscription_not_active:
      "Nur ein aktives Abonnement kann den Plan wechseln.",
  },
}
