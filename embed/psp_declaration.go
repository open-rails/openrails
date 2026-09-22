package embed

import "github.com/open-rails/openrails/internal/hosttools"

// PSPDeclaration supplies attribution for imported billing facts. It creates
// an identity without credentials and preserves all existing provider settings.
// Configure an active payment provider through MerchantConfig instead.
type PSPDeclaration = hosttools.PSPDeclaration
