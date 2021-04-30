from testmodule import *
import sys

class TestWrites(TestRunner):
		
	def __init__(self):
		super().__init__()
	
	def mthd(self):
	
		import pysharkbite
		
		securityOps = super().getSecurityOperations()

		securityOps.create_user("testUser","password")
		## validate that we DON'T see the permissions
		
		assert( False == securityOps.has_system_permission("testUser",pysharkbite.SystemPermissions.CREATE_TABLE) )
		securityOps.grant_system_permission("testUser",pysharkbite.SystemPermissions.CREATE_TABLE )
		securityOps.grant_system_permission("testUser",pysharkbite.SystemPermissions.DROP_TABLE )
		## validate that we DO see the permissions
		
		assert( True == securityOps.has_system_permission("testUser",pysharkbite.SystemPermissions.CREATE_TABLE) )
		
		auths = pysharkbite.Authorizations()
		auths.addAuthorization("blah1")
		auths.addAuthorization("blah2")
		
		securityOps.grantAuthorizations(auths,"testUser")


		tableOperations = super().getTableOperations()
		tableOperations.create(False)  

		## validate that we DO see the permissions


		securityOps.grant_table_permission("testUser",super().getTable(),pysharkbite.TablePermissions.READ )
		securityOps.grant_table_permission("testUser",super().getTable(),pysharkbite.TablePermissions.WRITE )
		securityOps.grant_table_permission("testUser",super().getTable(),pysharkbite.TablePermissions.DROP_TABLE )


		super().setUser("testUser","password")

		super().inity(replace=True)

		tableOperations = super().getTableOperations()
	
		securityOps = super().getConnector().securityOps()

		
		""" Add authorizations """ 
		""" mutation.put("cf","cq","cv",1569786960) """
		
		with tableOperations.createWriter(auths, 10) as writer:
			mutation = pysharkbite.Mutation("row2")
			mutation.put("cf","cq","blah1",1569786960, "value")
			mutation.put("cf2","cq2","blah1",1569786960, "value2")
			""" no value """
			mutation.put("cf3","cq3","blah2",1569786960, "") 
			writer.addMutation( mutation )
		
		
		auths = pysharkbite.Authorizations()
		
		auths.addAuthorization("blah1")
		
		scanner = tableOperations.createScanner(auths, 2)
		
		startKey = pysharkbite.Key()
		
		endKey = pysharkbite.Key()
		
		startKey.setRow("row")
		
		endKey.setRow("row3")
		
		range = pysharkbite.Range(startKey,True,endKey,False)
		
		scanner.addRange( range )
		
		resultset = scanner.getResultSet()
		
		for keyvalue in resultset:
			key = keyvalue.getKey()
			assert( "row2" == key.getRow() )
			value = keyvalue.getValue()
			if "cf" == key.getColumnFamily():
				assert( "value"  == value.get() )
			if ("cf2" == key.getColumnFamily() ):
				assert( "value2" == value.get() )
			if ("cf3" == key.getColumnFamily() ):
				print("Unexpected column cf3")
				sys.exit(1)
		    
		    
		
		""" delete your table if user did not create temp """
		
		tableOperations.remove()

		super().setUser("root","secret")
		super().inity(replace=True)
		tableOperations = super().getTableOperations()
	
		securityOps = super().getSecurityOperations()
		securityOps.remove_user("testUser")

		try:
			super().setUser("testUser","password")

			super().inity(replace=True)
			print("Expected failure when setting user")
			sys.exit(1)
		except pysharkbite.ClientException:
			print("caught expected exception")
			pass

runner = TestWrites()
runner.mthd()