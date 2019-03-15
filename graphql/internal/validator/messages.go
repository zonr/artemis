/**
 * Copyright (c) 2019, The Artemis Authors.
 *
 * Permission to use, copy, modify, and/or distribute this software for any
 * purpose with or without fee is hereby granted, provided that the above
 * copyright notice and this permission notice appear in all copies.
 *
 * THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL WARRANTIES
 * WITH REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF
 * MERCHANTABILITY AND FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR
 * ANY SPECIAL, DIRECT, INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES
 * WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR PROFITS, WHETHER IN AN
 * ACTION OF CONTRACT, NEGLIGENCE OR OTHER TORTIOUS ACTION, ARISING OUT OF
 * OR IN CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE.
 */

package validator

import (
	"fmt"
)

// DuplicateOperationNameMessage returns message describing error occurred in rule "Operation Name
// Uniqueness" (rules.UniqueOperationNames).
func DuplicateOperationNameMessage(operationName string) string {
	return fmt.Sprintf(`There can be only one operation named "%s".`, operationName)
}

// AnonOperationNotAloneMessage returns message describing error occurred in rule "Lone Anonymous
// Operation" (rules.LoneAnonymousOperation).
func AnonOperationNotAloneMessage() string {
	return "This anonymous operation must be the only defined operation."
}

// SingleFieldOnlyMessage returns message describing error occurred in rule "Single Field
// Subscriptions" (rules.SingleFieldSubscriptions).
func SingleFieldOnlyMessage(name string) string {
	if len(name) == 0 {
		return "Anonymous Subscription must select only one top level field."
	}
	return fmt.Sprintf(`Subscription "%s" must select only one top level field.`, name)
}
