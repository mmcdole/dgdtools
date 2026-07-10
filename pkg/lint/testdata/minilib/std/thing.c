/* Base object for the fixture lib. */

public int visible_fn();        /* forward decl, defined below: fine */
static void never_defined();    /* prototype with no definition anywhere */

private int _uses;

static void
create()
{
    _uses = 0;
}

public int
visible_fn()
{
    return 1;
}

static int
hidden_fn()
{
    return 2;
}

private int
secret_fn()
{
    return 3;
}
