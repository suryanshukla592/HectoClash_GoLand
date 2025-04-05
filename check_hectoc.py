# check_hectoc.py
import sys
import itertools

def evaluate(expression):
    try:
        return eval(expression)
    except (TypeError, ZeroDivisionError, SyntaxError):
        return None

def get_digit_splits(s):
    results = []
    n = len(s)

    def backtrack(index, path):
        if index == n:
            results.append(path[:])
            return
        for i in range(index + 1, n + 1):
            part = s[index:i]
            # Skip invalid numbers with leading zero
            if len(part) > 1 and part[0] == '0':
                continue
            path.append(int(part))
            backtrack(i, path)
            path.pop()

    backtrack(0, [])
    return results

def generate_expressions(digits, operators):
    def apply_ops(nums, ops_seq):
        if len(nums) == 1:
            yield str(nums[0])
            return

        for i in range(len(ops_seq)):
            op = ops_seq[i]
            left_nums = nums[:i+1]
            right_nums = nums[i+1:]
            left_ops = ops_seq[:i]
            right_ops = ops_seq[i+1:]

            for left_expr in apply_ops(left_nums, left_ops):
                for right_expr in apply_ops(right_nums, right_ops):
                    yield f"({left_expr}{op}{right_expr})"

    for group in get_digit_splits("".join(map(str, digits))):
        if len(group) < 2:
            continue
        for ops in itertools.product(operators, repeat=len(group) - 1):
            yield from apply_ops(group, list(ops))

def solve_hectoc_top1(digits):
    operators = ['+', '-', '*', '/']
    seen = set()
    for expr in generate_expressions(digits, operators):
        if expr in seen:
            continue
        seen.add(expr)
        result = evaluate(expr)
        if result is not None and abs(result - 100) < 1e-9:
            return True
    return False

if __name__ == "__main__":
    if len(sys.argv) != 2:
        print("Usage: python check_hectoc.py <digits>")
        sys.exit(1)

    input_str = sys.argv[1]
    if not input_str.isdigit() or len(input_str) != 6:
        print("Error: Input must be a 6-digit string of digits.")
        sys.exit(1)

    digits = list(map(int, list(input_str)))
    result = solve_hectoc_top1(digits)

    print("True" if result else "False")
