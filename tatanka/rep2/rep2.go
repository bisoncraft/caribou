package rep2

// Reputation dynamics:

// 1. The better your reputation is, the more a negative interaction will affect it
// 2. The better your reputation is, the less a positive interaction will affect it
// 3. The worse your reputation is, the more a positive interaction will affect it.
// 4. The worse your reputation is, the less a negative interaction will affect it.

// Impact Scale

// +1 nice!
//  0 meh, could've done better
// -1 could be an accident
// -2 ouch
// -3 what the heck?
// -4 I swear to god...
// -5 What a POS!

// At max reputation...
//     A -5 should take 25 off of your score
//     A +1 should do nothing
// At neutral reputation...
//     A -5 should take 5 off of your score
//     A +1 should add 1 to your scre
// At worst reputation...
//     A -5 can't do anything. We already hate you
//     A +1 should add 25 to your score. We can be magnanimous

// Auto-bonded is any score above 64. No bond is required. User can refund any existing bonds.

// Three classes of memories
//     Fleeting
//     Typical
//     Traumatic
