#include <stdio.h>

static int compute_total(int base, int count) {
    int multiplier = count - 3;  // Bug: expected count, not count - 3.
    int adjusted = base * multiplier;
    int result = adjusted + 7;
    return result;
}

int main(void) {
    int configured_count = 3;
    int total = compute_total(12, configured_count);
    printf("total=%d expected=43\n", total);
    return total == 43 ? 0 : 2;
}
