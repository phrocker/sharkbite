.. image:: https://camo.githubusercontent.com/dbf39cef1a973d8741437693e96b59e31d9e3754/68747470733a2f2f7777772e736861726b626974652e696f2f77702d636f6e74656e742f75706c6f6164732f323031372f30322f736861726b626974652e6a7067

Getting Statistics in Python
=============================

You can retreive Accumulo stats with the get_statistics function in the connector. 

.. code-block:: python

    from ctypes import cdll
    from argparse import ArgumentParser
    from ctypes import cdll
    import ctypes
    import traceback
    import json
    import time



    """

    This is an Example of using the Python connectors. The example will accept user input
    create a table writing arbitrary information to it via the BatchWriter and scanner will put the written data      
        
                
    """

    parser = ArgumentParser(description="This is an Apache Accummulo Python connector")

    parser.add_argument("-i", "--instance", dest="instance",
                        help="Apache Accumulo Instance Name", required=True)
    parser.add_argument("-z", "--zookeepers", dest="zookeepers",
                        help="Comma Separated Zookeeper List", required=True)
    parser.add_argument("-u", "--username", dest="username",
                        help="User to access Apache Accumulo", required=True)
    parser.add_argument("-p", "--password", dest="password",
                        help="Password to access Apache Accumulo. May also be supplied at the command line")
    parser.add_argument("-t", "--table", dest="table",
                        help="Table to create/update")
    args = parser.parse_args()

    password = args.password
    table = args.table

    if not password:
        print("Please enter your password")
        password = input()
        
    if not table:
        table = "blahblahd"

    import pysharkbite

    configuration = pysharkbite.Configuration()

    zk = pysharkbite.ZookeeperInstance(args.instance, args.zookeepers, 1000, configuration)

    user = pysharkbite.AuthInfo(args.username, password, zk.getInstanceId()) 

    try:
        connector = pysharkbite.AccumuloConnector(user, zk)

        stats = connector.getStatistics()

        print("Goal state: " + str(stats.goal_state))

        # print some server info
        for serverinfo in stats.tablet_server_info:
                print(serverinfo.name)
                print(serverinfo.last_contact)
                
        

      
    except RuntimeError as e:
        traceback.print_exc()
        print("Oops, error caused: " + str(e))
